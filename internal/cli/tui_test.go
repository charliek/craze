package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// stderrCanary is what the fake agent says on its stderr. It has to be a string
// craze itself would never draw.
const stderrCanary = "CANARY-AGENT-DIAGNOSTIC"

// TestTUIKeepsTheAgentStderrOffTheTerminal: while craze owns the alt screen
// nothing else may write to the terminal. cursor-agent writes diagnostics to its
// stderr whenever it likes, and internal/acp copies them straight through from a
// goroutine of its own, so handing it the real stderr puts them on top of a
// frame past every lock the renderer takes. They are buffered for the duration
// of the run and printed once the screen is back — buffered, not dropped.
func TestTUIKeepsTheAgentStderrOffTheTerminal(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Skipf("pty resize: %v", err)
	}

	// An "agent" whose first act is a diagnostic and which then stays alive, the
	// way a real one does: craze keeps the alt screen for the whole of it, which
	// is exactly the window the diagnostic must not use. The marker file says
	// the write has left the child, so the assertion below is not a race.
	dir := t.TempDir()
	bin := filepath.Join(dir, "noisy-agent")
	spoke := filepath.Join(dir, "spoke")
	script := "#!/bin/sh\necho " + stderrCanary + " >&2\ntouch " + spoke + "\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())

	tail := newPTYTail(ptmx)
	prevIn, prevOut, prevErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = tty, tty, tty
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = prevIn, prevOut, prevErr })

	done := make(chan error, 1)
	go func() { done <- runTUI(nil, &tuiFlags{agentBin: bin, workspace: dir, force: true}) }()

	if !tail.wait("\x1b[?1049h", 10*time.Second) {
		t.Fatalf("craze never entered the alt screen; got %q", tail.text())
	}
	// A frame is on the screen, and the agent has said its piece into the pipe.
	if !tail.wait(" craze ─", 10*time.Second) {
		t.Fatalf("craze never drew a frame; got %q", tail.text())
	}
	if !waitFile(spoke, 10*time.Second) {
		t.Fatal("the fake agent never wrote to its stderr")
	}
	time.Sleep(500 * time.Millisecond) // time for a leak to show up
	if strings.Contains(tail.text(), stderrCanary) {
		t.Fatalf("the agent wrote %q to the terminal while the TUI owned it", stderrCanary)
	}

	if _, err := ptmx.Write([]byte{0x04}); err != nil {
		t.Fatal(err)
	}
	select {
	// The fake agent never answered initialize, so whatever runTUI returns is
	// craze's exit status doing its job, not a test failure.
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("craze did not quit after ctrl+d")
	}
	if !tail.wait(stderrCanary, 10*time.Second) {
		t.Fatalf("the diagnostic was dropped instead of deferred; got %q", tail.text())
	}
}

// TestDeferredStderrCapsWhatItHolds: an agent looping on stderr must not grow
// the buffer without bound, and the cap has to say what it swallowed.
func TestDeferredStderrCapsWhatItHolds(t *testing.T) {
	d := &deferredStderr{}
	chunk := bytes.Repeat([]byte("x"), 4096)
	for written := 0; written < deferredStderrMax+2*len(chunk); written += len(chunk) {
		if n, err := d.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("write %d, %v", n, err)
		}
	}
	var out bytes.Buffer
	d.flush(&out)
	if got := strings.Count(out.String(), "x"); got != deferredStderrMax {
		t.Fatalf("kept %d bytes, want the cap %d", got, deferredStderrMax)
	}
	if !strings.Contains(out.String(), "dropped 8192 further bytes") {
		t.Fatalf("the cap is silent: %q", out.String()[deferredStderrMax:])
	}
	// A flush empties it: the note is not repeated on the next one.
	out.Reset()
	d.flush(&out)
	if out.Len() != 0 {
		t.Fatalf("a second flush wrote %q", out.String())
	}
}

// waitFile polls for a path, which is how the fake agent says it has written to
// its stderr without writing anywhere the test is watching.
func waitFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// ptyTail keeps reading the master side, because a test that stops reading
// stalls whatever is writing to the slave.
type ptyTail struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newPTYTail(ptmx *os.File) *ptyTail {
	tail := &ptyTail{}
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := ptmx.Read(tmp)
			if n > 0 {
				tail.mu.Lock()
				tail.buf.Write(tmp[:n])
				tail.mu.Unlock()
			}
			if err != nil && !os.IsTimeout(err) {
				return
			}
			if err == io.EOF {
				return
			}
		}
	}()
	return tail
}

func (p *ptyTail) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

func (p *ptyTail) wait(needle string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(p.text(), needle) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
