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
