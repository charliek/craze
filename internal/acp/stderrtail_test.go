package acp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	// The exact bytes whatever the machine's load: the reaper's drain, like
	// StderrTail's wait here, ends with the copy, not at its bound.
	t.Setenv(StderrWaitEnv, "10s")
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
// runs on until the reaper's group cleanup KILLs it. StderrTail, its wait up,
// answers with what has arrived — the agent's last line — while the copy is
// still running; the copy carries on unaffected, and what the tool writes
// afterwards reaches the sink and the tail. The call pending at the exit
// fails with the exit's status however the exit was observed: at once when
// the observation carried the status (X72) — always on Linux, and on macOS
// unless its zombie poll saw the exit before the kqueue's event did — and at
// the reap when it did not (X71), the path the second subtest forces
// (withoutStatus).
//
// Nothing here races the reaper (astra r11 6). The tool opens its FIFO only
// after its trap, and the test's end of it opens only once the tool's has
// (an open with no reader is ENXIO): that is the handshake that the tool
// ignores TERM, made before Initialize lets the agent exit. And the cleanup's
// group KILL is held (sendSignal) until the test has seen the tool's later
// line, so the reaper's grace orders nothing: the test waits for the reaper
// to reach that KILL — its TERM sent and ignored — and only then asks for the
// tail and lets the tool go. The grace's own expiry is the reaper's tests'
// (TestWhatTheAgentLeftBehindDiesAtItsExit).
//
// Nor does the test wait on what the held KILL holds up (astra r13 3): an
// exit observed without its status fails the pending call only at the reap,
// which comes after that KILL. So Initialize runs on a goroutine of its own,
// under a bound of its own, and the test goroutine reaches the KILL, asks
// for the tail and lets the tool and the KILL go without it, collecting its
// answer after. That the call failed before the reap is checked only where
// the status was out before the reaper reached its KILL. A failure anywhere
// lets the tool go, then the KILL, before the Close, so nothing is left
// running.
func TestStderrTailAnswersAtItsWait(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noStatus bool
	}{
		{"as the platform observes the exit", false},
		{"an exit observed without its status", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noStatus {
				withoutStatus(t)
			}
			gate := filepath.Join(t.TempDir(), "go")
			if err := syscall.Mkfifo(gate, 0o600); err != nil {
				t.Fatal(err)
			}
			// The hold on the cleanup's KILL, to this agent's group alone. Put
			// in before the Spawn, so it is taken out after the Close.
			var group atomic.Int64
			held, let := make(chan struct{}), make(chan struct{})
			var holdOnce, letOnce sync.Once
			letGo := func() { letOnce.Do(func() { close(let) }) }
			realSignal := sendSignal
			sendSignal = func(pid int, sig syscall.Signal) error {
				if sig == syscall.SIGKILL && pid == -int(group.Load()) {
					holdOnce.Do(func() { close(held) })
					<-let
				}
				return realSignal(pid, sig)
			}
			t.Cleanup(func() { sendSignal = realSignal })
			// Initialize's goroutine, joined last of all — after the Close, the
			// KILL's release and the FIFO's close, which are what let a pending
			// call end — so a failing subtest does not leave it running into
			// the next (astra r17).
			var initDone chan struct{}
			t.Cleanup(func() {
				if initDone == nil {
					return
				}
				select {
				case <-initDone:
				case <-time.After(10 * time.Second):
					t.Error("Initialize had not returned 10s into the cleanup")
				}
			})
			var sink syncBuffer
			// The tool ends with the FIFO, not on a timer: it opens the FIFO
			// once (fd 3: no second open to block on a writer already gone),
			// and its second read returns at the test's close of the write end
			// (an EOF), so it is gone even where no group KILL comes — the
			// fallback reaper sends none (astra r17) — and a first read that
			// meets that EOF ends it there too.
			c := spawnShell(t, `(trap '' TERM; exec 3< '`+gate+`'; read _ <&3 || exit 0; echo tool-later >&2; read _ <&3) & `+
				`read line; echo agent-last >&2; exit 7`, &sink)
			group.Store(int64(c.child.pgid))
			// After spawnShell's, so it runs before the Close: a failing test
			// lets the KILL go. openWhenRead's, after it, lets the tool go
			// first.
			t.Cleanup(letGo)
			fifo := openWhenRead(t, gate)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			initialized := make(chan error, 1)
			initDone = make(chan struct{})
			go func() {
				defer close(initDone)
				_, err := c.Initialize(ctx)
				initialized <- err
			}()
			failedWith7 := func(err error) {
				t.Helper()
				var ee *ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 7 {
					t.Fatalf("Initialize = %v, want it failed with the agent's exit status 7", err)
				}
			}
			sink.waitFor(t, "agent-last\n")
			select {
			case <-held:
			case <-time.After(20 * time.Second):
				t.Fatal("the reaper never reached its cleanup's KILL of the agent's group")
			}
			// The reaper is held before its reap. An observation that carried
			// the status published it before the cleanup began, and the call
			// pending at the exit fails with it then; one that did not leaves
			// both to the reap.
			statusAtExit := isClosed(c.child.statusCh)
			switch {
			case tc.noStatus && statusAtExit:
				t.Fatalf("a status (%v) published at an exit observed without one", c.child.exitErr)
			case !tc.noStatus && !statusAtExit && runtime.GOOS != "darwin":
				t.Fatal("the observation carried no status: off macOS it always does")
			case !tc.noStatus && !statusAtExit:
				t.Log("macOS observed the exit without its status: the call fails at the reap")
			}
			if statusAtExit {
				select {
				case err := <-initialized:
					failedWith7(err)
				case <-time.After(10 * time.Second):
					t.Fatal("the call pending at the exit did not fail at its status, the reaper held before its reap")
				}
			}
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
			if _, err := fifo.Write([]byte("\n")); err != nil {
				t.Fatalf("letting the tool go: %v", err)
			}
			sink.waitFor(t, "tool-later\n")
			letGo()
			if !statusAtExit {
				// The reap's status, within Initialize's bound.
				failedWith7(<-initialized)
			}
			_ = c.Close()
			if got := string(c.StderrTail(0)); got != "agent-last\ntool-later\n" {
				t.Fatalf("StderrTail after the reap = %q, want both lines", got)
			}
		})
	}
}

// openWhenRead is the write end of the FIFO at path, once something has its
// read end open — opened without blocking, so a reader that never comes fails
// the test rather than hanging it — and closed when the test ends.
func openWhenRead(t *testing.T, path string) *os.File {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			t.Cleanup(func() { _ = f.Close() })
			return f
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing opened %s to read: the tool never set its trap", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStderrWaitCanBeStretchedInATest: StderrWaitEnv stretches a wait for
// the copy of an agent's stderr, in a test process, to a positive duration it
// holds; anything else is the bound the caller gave.
func TestStderrWaitCanBeStretchedInATest(t *testing.T) {
	const bound = 500 * time.Millisecond
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"10s", 10 * time.Second},
		{"750ms", 750 * time.Millisecond},
		{"", bound},
		{"soon", bound},
		{"0s", bound},
		{"-1s", bound},
	} {
		t.Setenv(StderrWaitEnv, tc.env)
		if got := StderrWait(bound); got != tc.want {
			t.Fatalf("%s=%q: the wait is %v, want %v", StderrWaitEnv, tc.env, got, tc.want)
		}
	}
}
