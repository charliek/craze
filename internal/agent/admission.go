package agent

// AdmissionFence is a session that can start a turn of its own — the native
// session's wake, which delivers a background child's result (plan 026 §3.11)
// — and must not start one while the engine above it may be about to claim a
// turn, or has one. Like SubagentCanceller and LogOwner it is optional and found
// by a type assertion, so Session itself does not change; a session that never
// starts a turn of its own has no use for it.
//
// Up means the engine may be about to claim a turn or has one current: it is
// deciding an admission, a turn of its own is current, a cancel it validated is
// still on its way to the session, or a drain is owed at the end of the turn the
// agent is running now. While it is up the session must not start a turn of its
// own. Down means the engine is idle and owes nothing, so a turn the session
// starts cannot be mistaken for, or cancelled as, one of the engine's. It starts
// down; the engine's Stop and Close leave it up for good.
//
// The contract:
//
//   - The engine calls both under its own mutex (e.mu → s.mu, the order Begin and
//     ForeignTurn already keep), so an implementation takes only a leaf lock of
//     its own, never blocks, and never calls back into the engine: it records the
//     state, and on FenceDown it may signal a worker of its own without waiting.
//   - The calls are transitions: FenceUp is never called while the fence is up,
//     nor FenceDown while it is down.
//   - A section that reads ForeignTurn or calls Begin has raised it first, so a
//     session's own claim, made under its own lock only while the fence is down,
//     either happened before the engine's read — which then sees the agent's turn
//     and queues — or does not happen until the engine is idle again.
type AdmissionFence interface {
	FenceUp()
	FenceDown()
}

// OwedWork is a session that can owe its user work outside any turn — native's
// background children (plan 026 §3.11): a child still running, or its result
// published and waiting for the wake that delivers it. A host deciding whether
// its session is idle (plan 030 §3.6) reads it beside the roster and the
// agent's turn, which cannot say it: a background child's roster row is
// finished before its result is published (the harness reports the child's
// finish, then makes its result pending), and a pending result is no turn
// until the wake claims it — which a host's close fence, keeping the
// admission fence up, forbids (plan 030 C5r, astra r8-c5 1). Like
// AdmissionFence it is optional and found by a type assertion; a session
// without it owes nothing outside its turns.
//
// The contract: OwesWork takes only leaf locks and waits on nothing, and the
// engine never calls it holding its own mutex. Read with the admission fence
// up, its answer is a cut: nothing it counts can end without a turn — a
// running child becomes pending, and a pending result is taken only by a turn,
// which the fence keeps from starting.
type OwedWork interface {
	OwesWork() bool
}
