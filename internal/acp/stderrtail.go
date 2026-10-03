package acp

import (
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// stderrTailMax is how much of the agent's stderr Spawn keeps for
// Client.StderrTail: its last 2 KiB of raw bytes (plan 035 C7, SF-125).
const stderrTailMax = 2 << 10

// stderrTail is the pass-through recorder Spawn puts in front of the agent's
// stderr sink (SpawnOptions.Stderr): every write goes to the sink unchanged —
// the sink's answer is the copy's, so the copy behaves exactly as it did
// without the recorder — and the last stderrTailMax bytes written are kept in
// a ring, under a mutex, for what a failed start says (Client.StderrTail).
// Only the tail is kept: an agent's stderr can hold a token, and what reads
// the tail keeps less still.
type stderrTail struct {
	dst io.Writer

	mu   sync.Mutex
	ring [stderrTailMax]byte
	// end is how many bytes have been written in all: the ring holds the last
	// min(end, stderrTailMax) of them, the newest just before end's place in
	// it.
	end int64
}

func newStderrTail(dst io.Writer) *stderrTail { return &stderrTail{dst: dst} }

// Write records p, then hands it to the sink and answers as the sink does.
func (t *stderrTail) Write(p []byte) (int, error) {
	t.record(p)
	return t.dst.Write(p)
}

// record keeps p's bytes in the ring: all of them, or the last stderrTailMax
// of a longer p.
func (t *stderrTail) record(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if skip := len(p) - stderrTailMax; skip > 0 {
		t.end += int64(skip)
		p = p[skip:]
	}
	for len(p) > 0 {
		n := copy(t.ring[t.end%stderrTailMax:], p)
		t.end += int64(n)
		p = p[n:]
	}
}

// tail is a copy of what the ring holds, oldest byte first. When the ring has
// dropped bytes its first may be the middle of a UTF-8 rune, whose head went
// with them: those continuation bytes are dropped too, so the copy starts on
// a rune boundary rather than with bytes a reader would take for invalid
// text. Bytes the agent itself wrote invalid are kept as they are.
func (t *stderrTail) tail() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := min(t.end, stderrTailMax)
	out := make([]byte, n)
	start := (t.end - n) % stderrTailMax
	c := copy(out, t.ring[start:])
	copy(out[c:], t.ring[:start])
	if t.end > stderrTailMax {
		for i := 0; i < utf8.UTFMax-1 && len(out) > 0 && !utf8.RuneStart(out[0]); i++ {
			out = out[1:]
		}
	}
	return out
}

// StderrTail is the last 2 KiB of what the agent wrote to its stderr, raw:
// what a start that failed because its agent exited can say in the agent's
// own words (plan 035 C7). It waits up to wait for the copy of the agent's
// stderr to reach its end — the agent's last line can still be in the pipe
// when its exit is published (reaper.go) — and then answers a copy of the
// tail; a copy still running when wait is up (a process the agent left
// behind holding the pipe) is answered with what has arrived so far, and
// carries on unaffected. nil for an in-process test client with no child.
func (c *Client) StderrTail(wait time.Duration) []byte {
	if c.child == nil || c.child.stderrTail == nil {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-c.child.stderrDone:
	case <-timer.C:
	}
	return c.child.stderrTail.tail()
}
