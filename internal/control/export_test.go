package control

import "time"

// TestHooks are the server's barriers for tests outside the package (control_test):
// the hooks of the same names (server.go), set before Serve.
type TestHooks struct {
	// AdmissionFull runs on a reader that found every admission slot taken.
	AdmissionFull func()
	// BeforeUnbind runs on a closing connection's cleanup before its
	// compare-and-release, with its client id ("" before hello).
	BeforeUnbind func(client string)
	// BeforeBind runs in hello before the binding section.
	BeforeBind func()
	// BeforeBarrier runs on a handler after its command returned and before
	// the reply barrier.
	BeforeBarrier func(method string)
	// OutboxFull runs on a push that found no room and is about to wait.
	OutboxFull func()
}

// NewForTest is New with hooks in place, and the stall bound and outbound line
// limit lowered when stall or maxLine is positive.
func NewForTest(o Options, h TestHooks, stall time.Duration, maxLine int) *Server {
	s := New(o)
	s.hooks = hooks{
		admissionFull: h.AdmissionFull,
		beforeUnbind:  h.BeforeUnbind,
		beforeBind:    h.BeforeBind,
		beforeBarrier: h.BeforeBarrier,
		outboxFull:    h.OutboxFull,
	}
	if stall > 0 {
		s.stall = stall
	}
	if maxLine > 0 {
		s.maxLine = maxLine
	}
	return s
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
