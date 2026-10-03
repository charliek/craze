package acp

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The agent's stderr tail (plan 035 C7, SF-125): Spawn's recorder keeps the
// last 2 KiB of what the agent wrote to its stderr, passing every byte on to
// the sink, and Client.StderrTail answers it once the copy has reached its
// end — or, at its wait, what has arrived — for a failed start's words.

// failingWriter is a sink whose every write fails after taking n bytes.
type failingWriter struct{ n int }

func (w failingWriter) Write(p []byte) (int, error) {
	return min(w.n, len(p)), errors.New("the sink is gone")
}

// TestTheStderrRingKeepsTheLastBytes is the recorder alone: what it keeps
// after writes of every size — short ones, one that fills it exactly, one
// longer than it, many across its end — is the last stderrTailMax bytes
// written, in order; the sink gets every byte as written; and the sink's
// answer, an error included, is the write's, so the copy in front of it
// behaves exactly as it did without the recorder.
func TestTheStderrRingKeepsTheLastBytes(t *testing.T) {
	chunks := func(sizes ...int) [][]byte {
		var out [][]byte
		for i, n := range sizes {
			out = append(out, bytes.Repeat([]byte{'a' + byte(i%26)}, n))
		}
		return out
	}
	var many []int
	for range 100 {
		many = append(many, 37)
	}
	for _, tc := range []struct {
		name   string
		writes [][]byte
	}{
		{"nothing", nil},
		{"short writes", [][]byte{[]byte("one\n"), []byte("two\n")}},
		{"exactly full", chunks(stderrTailMax)},
		{"full, then one more byte", chunks(stderrTailMax, 1)},
		{"one write longer than the ring", chunks(3*stderrTailMax + 5)},
		{"many writes across its end", chunks(many...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink bytes.Buffer
			r := newStderrTail(&sink)
			var all []byte
			for _, p := range tc.writes {
				if n, err := r.Write(p); n != len(p) || err != nil {
					t.Fatalf("Write(%d bytes) = %d, %v", len(p), n, err)
				}
				all = append(all, p...)
			}
			want := all[max(0, len(all)-stderrTailMax):]
			if got := r.tail(); !bytes.Equal(got, want) {
				t.Fatalf("the tail is %d bytes %q…, want the last %d written", len(got), got[:min(len(got), 16)], len(want))
			}
			if !bytes.Equal(sink.Bytes(), all) {
				t.Fatalf("the sink got %d bytes, want every one of the %d written", sink.Len(), len(all))
			}
		})
	}
	t.Run("the sink's answer is the write's", func(t *testing.T) {
		r := newStderrTail(failingWriter{n: 3})
		if n, err := r.Write([]byte("hello")); n != 3 || err == nil || err.Error() != "the sink is gone" {
			t.Fatalf("Write = %d, %v; want the sink's 3, the sink is gone", n, err)
		}
		if got := string(r.tail()); got != "hello" {
			t.Fatalf("the tail is %q, want what the agent wrote, %q", got, "hello")
		}
	})
	t.Run("a rune its end cut in two goes", func(t *testing.T) {
		// 700 three-byte runes: the ring's last 2048 bytes begin one byte
		// into the 18th, whose two remaining bytes go too.
		r := newStderrTail(&bytes.Buffer{})
		_, _ = r.Write([]byte(strings.Repeat("日", 700)))
		if got, want := string(r.tail()), strings.Repeat("日", 682); got != want {
			t.Fatalf("the tail is %d bytes starting %q, want %d whole runes", len(got), got[:min(len(got), 6)], 682)
		}
	})
	t.Run("an agent's own invalid bytes stay", func(t *testing.T) {
		// Nothing dropped from the ring: a leading continuation byte is the
		// agent's, kept as it wrote it.
		r := newStderrTail(&bytes.Buffer{})
		_, _ = r.Write([]byte("\x80\x80oops"))
		if got := string(r.tail()); got != "\x80\x80oops" {
			t.Fatalf("the tail is %q, want the agent's bytes unchanged", got)
		}
	})
}

// TestStderrTailIsWhatTheAgentWrote (each shape the plan names): an agent
// that writes its stderr and exits — one line, two, CRLF ends, ANSI colour, a
// last fragment with no newline, one line over 2 KiB with no newline, a 2 KiB
// end that falls inside a three-byte rune, and nothing at all — is answered
// its raw last bytes once the copy is done, and the sink gets every byte.
func TestStderrTailIsWhatTheAgentWrote(t *testing.T) {
	long := strings.Repeat("x", 3000) + "END"
	runes := strings.Repeat("日", 700)
	for _, tc := range []struct {
		name, wrote, want string
	}{
		{"one line", "Error: one\n", "Error: one\n"},
		{"two lines", "Error: KEYCHAIN LOCKED\nRun unlock and retry.\n", "Error: KEYCHAIN LOCKED\nRun unlock and retry.\n"},
		{"CRLF", "first\r\nsecond\r\n", "first\r\nsecond\r\n"},
		{"ANSI", "\x1b[31mred\x1b[0m alert\n", "\x1b[31mred\x1b[0m alert\n"},
		{"a fragment with no newline", "done\npartial", "done\npartial"},
		{"one line over 2 KiB, no newline", long, long[len(long)-stderrTailMax:]},
		{"a cut inside a rune", runes, strings.Repeat("日", 682)},
		{"no stderr", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "said")
			if err := os.WriteFile(path, []byte(tc.wrote), 0o600); err != nil {
				t.Fatal(err)
			}
			var sink syncBuffer
			c := spawnShell(t, `cat '`+path+`' >&2; exit 1`, &sink)
			got := c.StderrTail(10 * time.Second)
			if string(got) != tc.want {
				t.Fatalf("StderrTail = %d bytes %q, want %d bytes %q", len(got), clip(string(got)), len(tc.want), clip(tc.want))
			}
			select {
			case <-c.child.stderrDone:
			default:
				t.Fatal("StderrTail answered before the copy ended, with nothing holding the pipe")
			}
			if sink.String() != tc.wrote {
				t.Fatalf("the sink got %d bytes, want every one of the %d the agent wrote", len(sink.String()), len(tc.wrote))
			}
		})
	}
	t.Run("an in-process client has none", func(t *testing.T) {
		if got := newClient(NewConn(strings.NewReader(""), &bytes.Buffer{}), nil).StderrTail(time.Second); got != nil {
			t.Fatalf("StderrTail = %q, want nil", got)
		}
	})
}

// clip is s cut for a failure message.
func clip(s string) string {
	if len(s) > 40 {
		return s[:20] + "…" + s[len(s)-20:]
	}
	return s
}

// TestStderrTailAnswersAtItsWait: a tool the agent left in its group, immune
// to SIGTERM, holds the stderr pipe open after the agent exits, so the copy
// runs on until the reaper's KILL a grace later. The call pending at the exit
// still fails at once with the exit's status (X72), and StderrTail, its wait
// up, answers with what has arrived — the agent's last line — while the copy
// is still running; the copy carries on unaffected, and what the tool writes
// afterwards reaches the sink and the tail.
func TestStderrTailAnswersAtItsWait(t *testing.T) {
	// The tool waits on a FIFO, not a poll: the test's one byte lets it go
	// at once, well inside the grace before its KILL.
	gate := filepath.Join(t.TempDir(), "go")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	var sink syncBuffer
	c := spawnShell(t, `(trap '' TERM; read _ < '`+gate+`'; echo tool-later >&2; sleep 60) & `+
		`read line; echo agent-last >&2; exit 7`, &sink)
	_, err := c.Initialize(t.Context())
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("Initialize = %v, want it failed with the agent's exit status 7", err)
	}
	sink.waitFor(t, "agent-last\n")
	const wait = 100 * time.Millisecond
	asked := time.Now()
	got := c.StderrTail(wait)
	if took := time.Since(asked); took < wait {
		t.Fatalf("StderrTail answered after %v, before its %v wait was up, with the copy still running", took, wait)
	}
	select {
	case <-c.child.stderrDone:
		t.Fatal("the copy had ended: the tool holding the pipe is gone, so this proves nothing")
	default:
	}
	if string(got) != "agent-last\n" {
		t.Fatalf("StderrTail = %q, want what had arrived, %q", got, "agent-last\n")
	}
	// Non-blocking: a tool already gone (no reader) is ENXIO, not a hang.
	fifo, err := os.OpenFile(gate, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("the tool is not waiting on its FIFO: %v", err)
	}
	if _, err := fifo.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	_ = fifo.Close()
	sink.waitFor(t, "tool-later\n")
	_ = c.Close()
	if got := string(c.StderrTail(0)); got != "agent-last\ntool-later\n" {
		t.Fatalf("StderrTail after the reap = %q, want both lines", got)
	}
}
