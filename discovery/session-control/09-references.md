# 09 — References

What was read in the 2026-09-19 discovery, and what to re-read when planning
a phase. Paths are relative to `~/projects/`. Line numbers drift; symbols and
file names are the durable part.

## craze itself

| what | where |
|---|---|
| The session interface, events, snapshot | `craze/internal/agent/session.go` (`Session`, `Event`, `EventType`, `Options`) |
| ACP-backed session: emit path, parked asks, lock-order comment | `craze/internal/agent/live.go` (`emit`/`emitCtx`, `waiting`, `onPermission`, `park`) |
| Native adapter and its sink | `craze/internal/agent/native.go` |
| Headless lifecycle driver | `craze/internal/cli/prompt.go` |
| Event JSON shapes, the wire schema's starting point | `craze/internal/cli/events.go` |
| Fan-out template | `craze/internal/host/hub.go` |
| Status derivation | `craze/internal/host/status.go` (`Derive`) |
| House IPC style: UDS, one NDJSON request per dial | `craze/internal/host/ndjson.go` |
| Structured transcript fused to rendering | `craze/internal/tui/transcript.go` |
| Turn choreography and cards that must move | `craze/internal/tui/app.go` (`turnSettled`, `finishTurn`, `cards`, `modeGen`, `writeIndex`, `waitEvent`, `sessionOwner`) |
| Session index | `craze/internal/sessions/sessions.go` |
| Harness store and its crash semantics | `craze/internal/harness/store/store.go` package doc |

## prox — tunnel and per-user daemon

`prox/internal/proxyd/`: `tunnel.go` (HTTP Upgrade → hijack → yamux, the
`hijackedConn` reader, clearing `ReadTimeout`, generation-guarded
attach/detach, the `OpenStream` no-context trap), `tunnel_client.go`
(reconnect loop, target allow-list), `hub.go` (listen-address classifier,
token file), `hub_server.go` (positive-list router), `daemon.go`
(`EnsureRunning`, stale sweep), `registry.go` (leases, grace, reservation),
`client.go` (`Proxy: nil`, no redirects, two clients over one transport).
`prox/internal/daemon/pidfile.go` (flock singleton).
`prox/docs/discovery/remote-proxy-hub.md`, `prox/docs/guides/remote-hub.md`.

Take: outbound dial, generations, leases, one goroutine owning the connection
state machine, never-fatal posture, controlled-side allow-list, protocol
integer. Leave: control plane on a second connection, the shared bearer token,
client-asserted origin, no message protocol. Size for calibration: hub +
tunnel about 2.8k lines, daemon singleton machinery about 1k, tests 1.26:1.
Every hard bug was a lifecycle bug (commits `a176d1b`, `c09451f`).

## shed, shed-mobile, roost, gx — the lane

| what | where |
|---|---|
| The lane contract; the module doc is the spec, including the corrections gx forced | `shed/crates/shed-core/src/lane.rs` |
| Published lane description, the page to hand an adapter author | `shed/docs/desktop/agent-lanes.md` |
| The shared fold/view | `shed/crates/shed-app/src/lane_view.rs` |
| gx adapter: fold, watcher, discovery and probe script, client pinning | `shed/crates/shed-gx/src/` |
| Executable spec: fakes with a control port | `shed/desktop/tools/shedtest/fake_gx.py`, `fake_lane_server.py` |
| The acceptance bar and standing rules | `shed/epics/roost-pivot.md` (also in shed-mobile) |
| Origin of the two-lane model; "t3code: learn, don't adopt" | `shed/docs/discovery/remote-agents.md` |
| UDS over an SSH exec, already working | `shed-mobile/lib/ssh/roost_tunnel.dart`, `duplex_pump.dart` |
| Lane kind dispatch | `shed-mobile/rust/src/api/lane.rs` (`LANE_KINDS`) |
| Lane UI and what it consumes | `shed-mobile/lib/features/lanes/lane_screen.dart`, `rust/src/api/dto_lane.rs` |
| craze as a roost agent; "reporting directly, without a hook" | `roost/docs/guides/agents.md` |
| gx's lane: endpoints, SSE resume rules, approvals lifecycle, verb × activity gate, accepted risks | `grok-build/docs/gx/REMOTE_API.md` |

## t3code — the destination shape

`thirdparty/t3code/`: `docs/internals/overview.md` and `glossary.md` (thread
durable, provider session detachable), `packages/contracts/src/orchestration.ts`
(commands, events, subscribe inputs), `apps/server/src/ws.ts` (subscribe
before snapshot, replay budgets in rows and bytes),
`apps/server/src/orchestration/LiveStreamBudget.ts` and
`ThreadLiveEventCoalescer.ts` (per-subscriber budgets, 50 ms coalescing),
`apps/server/src/provider/Services/ProviderAdapter.ts` (adapter SPI with
declared capabilities), `docs/internals/environment-auth.md` and `remote.md`
(scopes per RPC, pairing that narrows and never widens, endpoints as hints).

Take: durable thread with a detachable agent, snapshot + `afterSequence` +
`synchronized`, client command ids, asks as persisted state with a
blocked-on-you flag in the roster, per-subscriber budgets, capability flags
instead of version checks. Leave: Effect-grade layering, a dozen projection
tables, windowed pagination, DPoP and the relay product, server-owned PTYs.

## Sizes that shaped the estimates

| reference | non-test lines |
|---|---|
| craze `internal/agent` / `internal/tui` | 8.3k / 13.1k |
| shed-gx (fold 1.4k, watcher 1.2k, client 1.6k, discovery 0.8k) | about 6.6k |
| prox hub + tunnel / daemon singleton | 2.8k / 1k |
| gx `REMOTE_API.md` | 614 lines of spec for ten routes |
