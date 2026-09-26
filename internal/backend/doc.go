// Package backend is the TUI's seam onto a session (plan 027 §3.12): the
// engine surface the TUI actually uses, written in internal/engine,
// internal/agent and internal/transcript types, so that whatever serves a
// session can satisfy it structurally and neither side imports the other.
//
// Two things satisfy Backend:
//
//   - in process, the TUI's own engineBackend (internal/tui), which wraps the
//     *engine.Engine the TUI built and forwards every call to it, reading the
//     engine's primary directly (PR 3);
//   - over the control socket, remote.Session (internal/remote, PR 4), which
//     makes every call a round trip to the host that owns the engine.
//
// The interface is the permanent one (session control SD-33): every command
// and read takes a context.Context first, because over the socket every call
// is a round trip and the TUI's command gate carries the backend epoch in it
// (§3.12 "Chains are fenced"). In process the verbs that wait on nothing
// ignore it.
//
// It imports no client, no server and no wire (depguard's backend rule): not
// internal/tui or internal/cli, which build on it; not internal/control,
// internal/remote or internal/protocol, which are one implementation's
// transport; not internal/acp or internal/harness, which are below the
// session seam.
package backend
