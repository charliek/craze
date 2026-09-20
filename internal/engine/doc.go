// Package engine is the driver session control gives every client (plan 021
// §3.1). One engine wraps exactly one agent.Session and owns everything a
// client used to drive by hand: the turn driver (admission, settlement, the
// chain policy), the message queue and send-now, the ask registry's client
// side, settings order, command receipts, the index writes and the session's
// durable id. engine.Control is the surface every client holds — the TUI and
// craze prompt today, a socket server and its client later (SD-33) — so that
// two clients on one engine never double-drain and a session with no client
// still drains itself.
//
// Import boundary (a depguard rule on **/internal/engine/**, .golangci.yml):
// a non-test file here may not import internal/tui, internal/cli,
// internal/acp or internal/harness — the engine sits above the provider seam
// (agent.Session) and knows nothing of any one client or provider transport.
// A test file may additionally import internal/tui, because the package's
// own tests drive the engine over tui.Stub, an ordinary Go dependency an
// external test package (engine_test) is allowed to take on the package
// under test.
package engine
