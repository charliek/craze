# 05 — Protocol

A sketch to plan S2 from, not the spec. The spec is an S2 deliverable and
lives in `docs/` as published reference, with JSON Schema beside it.

## Shape (SD-03)

**JSON-RPC 2.0, one message per line (NDJSON), on one duplex stream.**
Requests and responses for commands; notifications for events. The same
stream runs unchanged over a Unix socket, over stdio through `craze bridge`,
and over a WebSocket (S6/S7).

Why this and not the alternatives:

| option | verdict |
|---|---|
| Own JSON-RPC over NDJSON | **Chosen.** craze's house style already (ACP to the agent, NDJSON to herdr and roost); one connection carries commands and the subscription; maps 1:1 onto every transport we need |
| HTTP + SSE over the socket (gx's shape) | Curl-debuggable and `Last-Event-ID` is standard, but needs several connections per client, each a separate SSH exec, and a second design for remote |
| ACP itself, extended | Would let ACP clients attach, but ACP has no multi-client attach, cursors, roster, queue, or addressable asks; extensions would dominate |

ACP **vocabulary** is reused where it exists: permission option kinds
(`allow_once`, `allow_always`, `reject_once`, `reject_always`), tool kinds,
stop reasons. shed's `LaneApprovalOption.kind` already uses the same words.

## Methods (sketch)

Every session-scoped method takes `sessionId`, even on a per-session socket
(SD-06). Every mutating method takes `commandId` (SD-11).

| method | notes |
|---|---|
| `hello` | protocol version (one integer), client kind, capabilities; reply carries host instance id, craze version, provider `Capabilities` |
| `sessions.list` / `sessions.subscribe` | roster rows: id, title, cwd, provider, model, activity, **pending ask count**, parent id, last change, host instance id. Readable without attaching to any session |
| `session.attach` | `{afterSeq?}` → `snapshot` (or replay), then events, then `synchronized` |
| `session.prompt` | `{text, mode: queue \| interject \| send_now}` |
| `session.cancel` | request/response; refused with `not_accepting` when there is nothing to cancel |
| `session.queue.*` | edit, remove, clear; mirrors `agent.Session` |
| `asks.list` / `asks.get` / `asks.answer` | by id; answer carries the exact offered `optionId`, or question answers, or a plan outcome |
| `session.set` | model, mode, config, title |
| `session.create` / `session.stop` | hub only (S4): spawn or end a headless host |

Notifications: `event` (seq + the event JSON, starting from
`internal/cli/events.go`'s shapes), `ask` (opened / submitted / resolved),
`roster`, `reset{reason}`.

## Attach and resume (SD-09)

1. Client sends `session.attach` with the last `seq` it has, if any.
2. The host **subscribes the client to live events first**, buffering, and
   only then reads the snapshot or replay. Otherwise events emitted during the
   read are lost (t3code spells this out in `apps/server/src/ws.ts`).
3. If `afterSeq` is recent enough, replay from the journal; the decision is
   bounded by **bytes as well as rows**, because a few large tool payloads
   dominate. Otherwise send a fresh snapshot stamped with its `seq`.
4. Deliver the buffered events, dropping any with `seq` at or below what was
   sent, then `synchronized`.
5. A cursor the host cannot honor gets an explicit `reset{reason}`
   (`cursor_unresolvable`, `slow_consumer`), never a silent gap.

Unlike gx, **asks and roster changes are sequenced in the same journal**, so
they resume exactly. gx lists "an API-owned journal" as future work and shed
papers over its absence with a re-fetch and tombstones on every reconnect.

## The verb × activity gate

Published in the spec as a table, enforced by the engine (`03`):

| activity | `queue` | `interject` | `send_now` | `cancel` |
|---|---|---|---|---|
| working | allow | allow if the provider can | allow | allow |
| blocked on an ask | allow | `not_accepting` | `not_accepting` | allow |
| idle | allow | `not_accepting` | allow | `not_accepting` |

Provider limits come from `Capabilities` in `hello`, so a client hides what a
provider cannot do instead of discovering it by error.

## Errors

`{code, message}` with a closed set of string codes that map onto shed's
`LaneError`: `bad_request`, `unknown_session`, `unknown_ask`,
`already_submitted`, `already_resolved`, `not_accepting`, `unsupported`,
`unavailable`. No client should ever match on message text.

## Compatibility rules

- One protocol-version integer, decoupled from the binary version (prox's
  `HubProtocolVersion`). The hub and hosts negotiate; a client never assumes.
- **Tolerant inbound, strict outbound**, shed's asymmetric rule: clients
  ignore unknown event kinds and fields; hosts reject unknown commands and
  option ids. shed-mobile pins by git rev and lags, so this is what lets the
  host add an event kind without a coordinated release.
- New behavior is announced as a capability in `hello`, never inferred from
  the version.
- Payloads that must cross shed's FRB bridge are owned scalars, strings,
  options, and lists; free-form payloads travel as a **string holding JSON**
  (`06`).

## `craze bridge` (SD-05)

`craze bridge [--session <id>]` pumps stdin/stdout to the right local socket
and exits when either side closes. It resolves the target itself: the hub
when one is running, otherwise `run/host/<id>.sock`. SSH clients run it as an
exec command, the way shed-mobile already runs `roost-session client-bridge`
for roost. It composes nothing from its input and prints nothing but protocol
bytes on stdout.

## Executable spec (SD-14)

- JSON Schema for every message, generated from or checked against the Go
  types, in the repo.
- Golden transcripts: recorded sessions as NDJSON fixtures, including resume,
  a slow-consumer reset, a double-answered ask, and a cancel on idle.
- A **fake host** scripted by fixtures, with knobs on a control channel,
  modeled on shed's `desktop/tools/shedtest/fake_gx.py`. The wire vocabulary
  lives in exactly one place; a client test that re-derives an envelope would
  agree with its own mistake.
- `shed-craze` (Rust, S3) is the second implementation that proves the
  contract rather than the plumbing.
