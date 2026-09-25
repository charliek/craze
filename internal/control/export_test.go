package control

import (
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// TestHooks are the server's barriers for tests outside the package (control_test):
// the hooks of the same names (server.go), set before Serve.
type TestHooks struct {
	// BeforeAcquire runs on a reader before it takes each admission slot, with
	// the connection's id (the order the server accepted it in, from 1).
	BeforeAcquire func(conn uint64)
	// AdmissionFull runs on a reader that found every admission slot taken.
	AdmissionFull func()
	// BeforeUnbind runs on a closing connection's cleanup before its
	// compare-and-release, with its client id ("" before hello).
	BeforeUnbind func(client string)
	// BeforeBind runs in hello before the binding section.
	BeforeBind func()
	// BeforeCommand runs on a handler about to run a mutating command, before
	// the host-wide cap and the binding's re-check.
	BeforeCommand func(method string)
	// BeforeBarrier runs on a handler after its command returned and before
	// the reply barrier.
	BeforeBarrier func(method string)
	// OutboxFull runs on a push that found no room and is about to wait.
	OutboxFull func()
	// BeforeForward runs on a forwarder just before it queues the event with
	// seq, with the subscription's id: a test that blocks in it holds the
	// forwarder unscheduled.
	BeforeForward func(sub string, seq uint64)
	// BeforeTerminal runs on a forwarder whose subscription has ended, just
	// before it claims and queues its final records and reset.
	BeforeTerminal func(sub string, reason protocol.ResetReason)
	// BarrierWaits runs on a handler whose reply barrier is about to wait for
	// the connection's attachment, with the seq it waits for.
	BarrierWaits func(seq uint64)
	// ReadyOwed runs on a ready watcher once it has handed the forwarder its
	// ready notification, with the seq it waits behind.
	ReadyOwed func(sub string, seq uint64)
}

// NewForTest is New with hooks in place, and the stall bound and outbound line
// limit lowered when stall or maxLine is positive.
func NewForTest(o Options, h TestHooks, stall time.Duration, maxLine int) *Server {
	s := New(o)
	s.hooks = hooks{
		beforeAcquire:  h.BeforeAcquire,
		admissionFull:  h.AdmissionFull,
		beforeUnbind:   h.BeforeUnbind,
		beforeBind:     h.BeforeBind,
		beforeCommand:  h.BeforeCommand,
		beforeBarrier:  h.BeforeBarrier,
		outboxFull:     h.OutboxFull,
		beforeForward:  h.BeforeForward,
		beforeTerminal: h.BeforeTerminal,
		barrierWaits:   h.BarrierWaits,
		readyOwed:      h.ReadyOwed,
	}
	if stall > 0 {
		s.stall = stall
	}
	if maxLine > 0 {
		s.maxLine = maxLine
	}
	return s
}

// SetAcceptBackoff sets the accept loop's backoff bounds (5 ms and 1 s in
// production), before Serve.
func (s *Server) SetAcceptBackoff(lo, hi time.Duration) { s.backoffMin, s.backoffMax = lo, hi }

// SetReadyWait sets a when: "ready" attach's bound on its wait for the
// session's start (10 minutes in production), before Serve.
func (s *Server) SetReadyWait(d time.Duration) { s.readyWait = d }

// Bindings is how many bindings the table holds and how many tokens the index
// holds.
func (s *Server) Bindings() (binds, tokens int) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	return len(s.binds), len(s.tokens)
}

// OutboxHighWater is the most bytes any live connection's outbox has held.
func (s *Server) OutboxHighWater() int {
	high := 0
	for _, c := range s.liveConns() {
		high = max(high, c.out.highWater())
	}
	return high
}

// Admitted is, per live connection, how many requests are admitted and their
// replies not yet written.
func (s *Server) Admitted() []int {
	var out []int
	for _, c := range s.liveConns() {
		c.mu.Lock()
		out = append(out, c.inflight)
		c.mu.Unlock()
	}
	return out
}

// Handlers is how many handler goroutines are running.
func (s *Server) Handlers() int { return s.handlers.running() }

// Commands is how many mutating commands are in their engine call.
func (s *Server) Commands() int64 { return s.commands.Load() }

// SessionCapabilities is sessionCapabilities, for the capability mapping's
// reflection test.
var SessionCapabilities = sessionCapabilities
