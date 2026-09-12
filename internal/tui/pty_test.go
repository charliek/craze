package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

func TestPTYAltScreenAndCtrlDQuit(t *testing.T) {
	isolateSkillsHome(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Skipf("pty resize: %v", err)
	}

	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
	})
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(tty), tea.WithOutput(tty))
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	sawAlt := false
	for time.Now().Before(deadline) && !sawAlt {
		_ = ptmx.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, rerr := ptmx.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if bytes.Contains(buf.Bytes(), []byte("\x1b[?1049h")) {
				sawAlt = true
				break
			}
		}
		if rerr != nil && !os.IsTimeout(rerr) && rerr != io.EOF {
			t.Fatalf("read pty: %v", rerr)
		}
	}
	if !sawAlt {
		t.Fatalf("did not observe alt-screen enter; got %q", buf.Bytes())
	}

	if _, err := ptmx.Write([]byte{0x04}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		p.Kill()
		t.Fatal("TUI did not quit after ctrl+d")
	}
}

// TestPTYWheelScrollsTranscript drives the real escape sequence a terminal
// sends for a wheel-up notch, which is the only thing a PTY can prove about
// the mouse.
func TestPTYWheelScrollsTranscript(t *testing.T) {
	isolateSkillsHome(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Skipf("pty resize: %v", err)
	}

	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
	})
	for i := 0; i < 60; i++ {
		m.addNote(fmt.Sprintf("scrollback line %02d", i))
	}
	p := tea.NewProgram(m,
		tea.WithAltScreen(), tea.WithMouseCellMotion(),
		tea.WithInput(tty), tea.WithOutput(tty))
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()
	defer func() {
		p.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			p.Kill()
		}
	}()

	if !waitPTY(t, ptmx, "scrollback line 59", 3*time.Second) {
		t.Fatal("the bottom of the transcript never rendered")
	}
	// Enough notches to bring the very first line back into view.
	for i := 0; i < 20; i++ {
		if _, err := ptmx.Write([]byte("\x1b[<64;10;5M")); err != nil {
			t.Fatal(err)
		}
	}
	if !waitPTY(t, ptmx, "scrollback line 00", 3*time.Second) {
		t.Fatal("the wheel sequence did not scroll the transcript")
	}
}

// waitPTY reads until needle shows up in the stream or the deadline passes.
func waitPTY(t *testing.T, ptmx *os.File, needle string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for time.Now().Before(deadline) {
		_ = ptmx.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := ptmx.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if bytes.Contains(buf.Bytes(), []byte(needle)) {
				return true
			}
		}
		if err != nil && !os.IsTimeout(err) && err != io.EOF {
			t.Fatalf("read pty: %v", err)
		}
	}
	return false
}
