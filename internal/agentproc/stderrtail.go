package agentproc

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// StderrTailMax is how much of the agent's stderr Start keeps for
// Child.StderrTail: its last 2 KiB of raw bytes (plan 035 C7, SF-125).
const StderrTailMax = 2 << 10

type stderrTail struct {
	dst io.Writer

	mu   sync.Mutex
	ring [StderrTailMax]byte
	end  int64
}

func newStderrTail(dst io.Writer) *stderrTail { return &stderrTail{dst: dst} }

func (t *stderrTail) Write(p []byte) (int, error) {
	t.record(p)
	return t.dst.Write(p)
}

func (t *stderrTail) record(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if skip := len(p) - StderrTailMax; skip > 0 {
		t.end += int64(skip)
		p = p[skip:]
	}
	for len(p) > 0 {
		n := copy(t.ring[t.end%StderrTailMax:], p)
		t.end += int64(n)
		p = p[n:]
	}
}

func (t *stderrTail) tail() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := min(t.end, StderrTailMax)
	out := make([]byte, n)
	start := (t.end - n) % StderrTailMax
	c := copy(out, t.ring[start:])
	copy(out[c:], t.ring[:start])
	if t.end > StderrTailMax {
		for i := 0; i < utf8.UTFMax-1 && len(out) > 0 && !utf8.RuneStart(out[0]); i++ {
			out = out[1:]
		}
	}
	return out
}

// StderrWaitEnv stretches, in a test process, every bounded wait for the copy
// of an agent's stderr to reach its end to the duration it holds.
const StderrWaitEnv = "CRAZE_TEST_STDERR_TAIL_WAIT"

// StderrWait is d, a bound on a wait for the copy of an agent's stderr to
// reach its end — or, in a test process, StderrWaitEnv's duration.
func StderrWait(d time.Duration) time.Duration {
	if testing.Testing() {
		if v, err := time.ParseDuration(os.Getenv(StderrWaitEnv)); err == nil && v > 0 {
			return v
		}
	}
	return d
}

// StderrDropped is how many chunks of the agent's stderr an AsyncStderr sink
// dropped because its sink was behind; 0 when AsyncStderr is false.
func (ch *Child) StderrDropped() int64 {
	if ch == nil || ch.stderrSink == nil {
		return 0
	}
	return ch.stderrSink.dropped.Load()
}

// StderrTail is the last 2 KiB of what the agent wrote to its stderr, raw.
func (ch *Child) StderrTail(wait time.Duration) []byte {
	if ch == nil || ch.stderrTail == nil {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch.stderrDone:
	case <-timer.C:
	}
	return ch.stderrTail.tail()
}

const asyncSinkQueue = 256

type asyncSink struct {
	dst     io.Writer
	ch      chan []byte
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64
}

func newAsyncSink(dst io.Writer) *asyncSink {
	a := &asyncSink{dst: dst, ch: make(chan []byte, asyncSinkQueue), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		for b := range a.ch {
			_, _ = a.dst.Write(b)
		}
	}()
	return a
}

func (a *asyncSink) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	select {
	case a.ch <- b:
	default:
		a.dropped.Add(1)
	}
	return len(p), nil
}

func (a *asyncSink) closeWithin(d time.Duration) {
	a.once.Do(func() { close(a.ch) })
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.done:
	case <-t.C:
	}
}
