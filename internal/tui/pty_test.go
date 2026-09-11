package tui

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

func TestPTYAltScreenAndQuit(t *testing.T) {
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

	if _, err := ptmx.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		p.Kill()
		t.Fatal("TUI did not quit after q")
	}
}
