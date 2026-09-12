package tui

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

func TestWiredFakeAgentStreamFollowUpQuit(t *testing.T) {
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
	if runtime.GOOS == "linux" && !processRunning(bin) {
		t.Fatal("expected fake-agent child after Start")
	}

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "default"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	m.started = true

	m.input.SetValue("one")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected prompt cmd")
	}
	doneMsg := cmd()
	m = drainEvents(t, m, sess)
	tm, _ = m.Update(doneMsg)
	m = tm.(Model)
	if !strings.Contains(m.View(), "first reply") {
		t.Fatalf("missing first stream chunk:\n%s", m.View())
	}

	m.input.SetValue("two")
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	doneMsg = cmd()
	m = drainEvents(t, m, sess)
	tm, _ = m.Update(doneMsg)
	m = tm.(Model)
	if !strings.Contains(m.View(), "second reply") {
		t.Fatalf("missing follow-up:\n%s", m.View())
	}

	tm, qcmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting || qcmd == nil {
		t.Fatal("expected quit")
	}
	msg := qcmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit cmd returned %T, want tea.QuitMsg", msg)
	}

	if runtime.GOOS == "linux" {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if !processRunning(bin) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("fake-agent child still running after quit/close")
	}
}

func TestWiredQuitWhileWorkingReapsChild(t *testing.T) {
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
		_ = promptCmd()
	}()

	tm, qcmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting || qcmd == nil {
		t.Fatal("expected quit")
	}

	quitDone := make(chan tea.Msg, 1)
	go func() { quitDone <- qcmd() }()
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

	if runtime.GOOS == "linux" && processRunning(bin) {
		t.Fatal("fake-agent child still running after quit while working")
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

func buildFakeAgent(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir := t.TempDir()
	bin := filepath.Join(dir, "craze-fake-agent-wire")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/craze-fake-agent")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fake agent: %v\n%s", err, out)
	}
	return bin
}

func processRunning(bin string) bool {
	entries, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		return false
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
