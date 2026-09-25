package engine

import "github.com/charliek/craze/internal/agent"

// GateTableHooks are the two barriers TestTheGateTableIsTheEngines needs from
// outside the package (gate_table_test.go is in engine_test, because it drives
// the engine over tui.Stub, which imports this package): BeforeSessionCancel
// holds a cancel after its hold is taken and before the session hears of it —
// the "cancelling" state, and an armed send-now's cancel kept from settling
// the turn it was armed against — and TurnReturned says a continuation has come
// back and the pass it allowed is over, which is when a refused claim is
// waiting (State.Waiting). They are the hooks of the same names, and exist in
// test builds only.
type GateTableHooks struct {
	BeforeSessionCancel func(turn string)
	TurnReturned        func(turn string)
}

// NewForGateTable is New with GateTableHooks in place from birth, which is the
// only way the driver's goroutine may be given any.
func NewForGateTable(sess agent.Session, opts Options, h GateTableHooks) (*Engine, error) {
	return newEngine(sess, opts, &hooks{
		beforeSessionCancel: h.BeforeSessionCancel,
		turnReturned:        h.TurnReturned,
	})
}
