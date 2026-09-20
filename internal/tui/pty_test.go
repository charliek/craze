package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"

	"github.com/charliek/craze/internal/agent"
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
	drainPTY(ptmx)

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
		drainPTY(ptmx)
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

// TestPTYCardsAnswerFromARealTerminal drives the two blocking cards against
// the scripted agent through a PTY, where the keys arrive as the bytes a
// terminal actually sends.
func TestPTYCardsAnswerFromARealTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		// steps are (wait for this, then type that) pairs.
		steps [][2]string
		want  string
	}{
		{
			name:   "ask",
			script: "ask",
			steps: [][2]string{
				{"question 1/2", "1"},
				{"question 2/2", " 3\r"},
			},
			want: "asked:answered:q1=opt-a;q2=opt-x,opt-z",
		},
		{
			name:   "plan",
			script: "plan",
			steps:  [][2]string{{"[a]ccept", "a"}},
			want:   "planned:accepted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := buildFakeAgent(t)
			isolateSkillsHome(t)
			ptmx, tty, err := pty.Open()
			if err != nil {
				t.Skipf("no pty: %v", err)
			}
			defer func() { _ = ptmx.Close() }()
			defer func() { _ = tty.Close() }()
			if err := pty.Setsize(tty, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
				t.Skipf("pty resize: %v", err)
			}

			ws := frameWorkspace(t)
			sess := agent.New(agent.Options{
				Binary:      bin,
				ExtraArgs:   []string{"-script=" + tc.script},
				Workspace:   ws,
				Force:       true,
				Interactive: true,
				Stderr:      io.Discard,
			})
			m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true})
			p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(tty), tea.WithOutput(tty))
			done := make(chan error, 1)
			go func() {
				_, err := p.Run()
				done <- err
			}()
			defer func() {
				drainPTY(ptmx)
				p.Quit()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					p.Kill()
				}
				_ = sess.Close()
			}()

			if !waitPTY(t, ptmx, "cursor", 10*time.Second) {
				t.Fatal("craze never started")
			}
			if _, err := ptmx.Write([]byte("go\r")); err != nil {
				t.Fatal(err)
			}
			for _, step := range tc.steps {
				if !waitPTY(t, ptmx, step[0], 10*time.Second) {
					t.Fatalf("card never showed %q", step[0])
				}
				if _, err := ptmx.Write([]byte(step[1])); err != nil {
					t.Fatal(err)
				}
			}
			if !waitPTY(t, ptmx, tc.want, 10*time.Second) {
				t.Fatalf("the agent never saw the answer %q", tc.want)
			}
		})
	}
}

// TestPTYShellModeRunsAndDrawsItsOutput drives the composer's `!` from a real
// terminal: the keys arrive as the bytes a terminal sends, the command runs in
// a process group of its own under the program's own event loop, and its output
// is drawn into the transcript.
//
// The assertion is the *lowercased* output and not the command, because a
// needle the user typed would already be in the stream as the draft. It is also
// one segment of one row: the head row is drawn as three styled segments, so
// "✓ ! echo" never appears contiguously in what a terminal receives.
func TestPTYShellModeRunsAndDrawsItsOutput(t *testing.T) {
	isolateSkillsHome(t)
	t.Setenv("SHELL", "/bin/sh")
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
	defer func() {
		drainPTY(ptmx)
		p.Quit()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			p.Kill()
		}
	}()

	if !waitPTY(t, ptmx, strings.Repeat("─", 72)+" craze ─", 5*time.Second) {
		t.Fatal("craze never drew its composer")
	}
	if _, err := ptmx.Write([]byte("!echo CRAZEPTYSHELL | tr A-Z a-z\r")); err != nil {
		t.Fatal(err)
	}
	if !waitPTY(t, ptmx, "crazeptyshell", 20*time.Second) {
		t.Fatal("the command's output never reached the transcript")
	}
}

// drainPTY keeps emptying the master once a test has finished asserting on it.
// A test that stops reading stalls whatever is writing to the slave, and the
// last thing bubbletea does on the way out of Program.Run is flush the final
// frame from inside renderer.stop() — so an unread master turns "quit" into a
// deadlock rather than a slow exit. Linux happens to buffer a whole 24x80
// styled frame; darwin's tty output queue is a few KiB and does not. Call it
// after the last waitPTY, never alongside one: two readers on one descriptor
// would split the stream between them.
func drainPTY(ptmx *os.File) {
	_ = ptmx.SetReadDeadline(time.Time{})
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()
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

// TestPTYSyncWriterKeepsTheWindowSize is the regression the writer nearly was.
// bubbletea only finds the terminal size through an output it can take a file
// descriptor from: wrapping stdout in a plain io.Writer leaves it with no
// window size at all, so craze would never receive a WindowSizeMsg and would
// draw nothing. The wrapper has to stay a file to bubbletea.
func TestPTYSyncWriterKeepsTheWindowSize(t *testing.T) {
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

	out := newSyncWriter(tty)
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
	})
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(tty), tea.WithOutput(out))
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()
	defer func() {
		drainPTY(ptmx)
		p.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			p.Kill()
		}
	}()

	// The composer rule is only drawn once the model has a width, and it is
	// drawn to exactly that width: 80 cells, the last eight of which are the
	// placeholder title.
	if !waitPTY(t, ptmx, strings.Repeat("─", 72)+" craze ─", 5*time.Second) {
		t.Fatal("no frame at the pty's own size: bubbletea lost the descriptor")
	}
}
