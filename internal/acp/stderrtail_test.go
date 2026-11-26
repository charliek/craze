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

	"github.com/charliek/craze/internal/agentproc"
)

func TestStderrTailIsWhatTheAgentWrote(t *testing.T) {
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
		{"one line over 2 KiB, no newline", long, long[len(long)-agentproc.StderrTailMax:]},
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
			case <-c.child.StderrDone():
			default:
				t.Fatal("StderrTail answered before the copy ended")
			}
			if sink.String() != tc.wrote {
				t.Fatalf("the sink got %d bytes, want %d", len(sink.String()), len(tc.wrote))
			}
		})
	}
	t.Run("an in-process client has none", func(t *testing.T) {
		if got := newClient(NewConn(strings.NewReader(""), &bytes.Buffer{}), nil).StderrTail(time.Second); got != nil {
			t.Fatalf("StderrTail = %q, want nil", got)
		}
	})
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:20] + "…" + s[len(s)-20:]
	}
	return s
}

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
			var group atomic.Int64
			held, let := make(chan struct{}), make(chan struct{})
			var holdOnce, letOnce sync.Once
			letGo := func() { letOnce.Do(func() { close(let) }) }
			restoreSig := agentproc.SetSendSignalForTest(func(pid int, sig syscall.Signal) error {
				if sig == syscall.SIGKILL && pid == -int(group.Load()) {
					holdOnce.Do(func() { close(held) })
					<-let
				}
				return syscall.Kill(pid, sig)
			})
			t.Cleanup(restoreSig)
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
			c := spawnShell(t, `(trap '' TERM; exec 3< '`+gate+`'; read _ <&3 || exit 0; echo tool-later >&2; read _ <&3) & `+
				`read line; echo agent-last >&2; exit 7`, &sink)
			group.Store(int64(c.child.PGID()))
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
					t.Fatalf("Initialize = %v, want exit status 7", err)
				}
			}
			sink.waitFor(t, "agent-last\n")
			select {
			case <-held:
			case <-time.After(20 * time.Second):
				t.Fatal("the reaper never reached its cleanup's KILL")
			}
			statusAtExit := isClosedCh(c.child.StatusReady())
			switch {
			case tc.noStatus && statusAtExit:
				t.Fatalf("a status (%v) published at an exit observed without one", c.child.ExitErr())
			case !tc.noStatus && !statusAtExit && runtime.GOOS != "darwin":
				t.Fatal("the observation carried no status")
			case !tc.noStatus && !statusAtExit:
				t.Log("macOS observed the exit without its status: the call fails at the reap")
			}
			if statusAtExit {
				select {
				case err := <-initialized:
					failedWith7(err)
				case <-time.After(10 * time.Second):
					t.Fatal("the call pending at the exit did not fail at its status")
				}
			}
			const wait = 100 * time.Millisecond
			asked := time.Now()
			got := c.StderrTail(wait)
			if took := time.Since(asked); took < wait {
				t.Fatalf("StderrTail answered after %v, before its %v wait was up", took, wait)
			}
			select {
			case <-c.child.StderrDone():
				t.Fatal("the copy had ended")
			default:
			}
			if string(got) != "agent-last\n" {
				t.Fatalf("StderrTail = %q, want %q", got, "agent-last\n")
			}
			if _, err := fifo.Write([]byte("\n")); err != nil {
				t.Fatalf("letting the tool go: %v", err)
			}
			sink.waitFor(t, "tool-later\n")
			letGo()
			if !statusAtExit {
				failedWith7(<-initialized)
			}
			_ = c.Close()
			if got := string(c.StderrTail(0)); got != "agent-last\ntool-later\n" {
				t.Fatalf("StderrTail after the reap = %q, want both lines", got)
			}
		})
	}
}

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
			t.Fatalf("nothing opened %s to read", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
