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
| HTTP + SSE over the socket (gx's shape) | Curl-debuggable and `Last-Event-ID` is standard, but it splits one session over a request channel and an event channel, and would need a second design for the stdio bridge |
| ACP itself, extended | Would let ACP clients attach, but ACP has no multi-client attach, cursors, roster, queue, or addressable asks; extensions would dominate |

ACP **vocabulary** is reused where it exists: permission option kinds
(`allow_once`, `allow_always`, `reject_once`, `reject_always`), tool kinds,
stop reasons. shed's `LaneApprovalOption.kind` already uses the same words.

## Methods (sketch)

Every session-scoped method takes `sessionId`, even on a per-session socket
(SD-06). Every mutating method takes `commandId` (SD-11), which is not the
JSON-RPC request id. Capabilities come at two levels (SD-28): the connection's
(`hello`) and each session's (its roster row and attach reply), because a hub
fronts hosts with different providers and versions.

Event payloads use the **lossless event codec** from S1a (`03`, SD-20), not
`craze prompt --json`'s shapes, which drop ask ids, option kinds, plan bodies,
and more.

| method | notes |
|---|---|
| `hello` | protocol version (one integer), client kind, client capabilities; reply carries the endpoint's identity (host or hub), craze version, connection-level capabilities, the command retry horizon |
| `sessions.list` / `sessions.subscribe` | roster rows: id, title, cwd, provider, model, activity, **pending ask count**, parent id, last change, host incarnation, session capabilities. Readable without attaching. The roster has its **own epoch and cursor**; a new epoch means reseed |
| `session.attach` | `{cursor?: {incarnation, seq}}` → a **subscription id**, then `snapshot` (or replay), events, `synchronized`. `session.detach{subscriptionId}` ends it; closing the connection ends all of them and **never** stops the session |
| `session.prompt` | `{text, mode: queue \| interject \| send_now}`; admitted by the engine's turn driver, answered with the engine turn id it became or queued behind |
| `session.cancel` | `{turnId?}` request/response: `rejected` (`not_accepting`, or the named turn is no longer current), `requested`, `settled`, or `unknown` after a timeout past the write |
| `session.queue.*` | edit, remove, clear; mirrors `agent.Session` |
| `asks.list` / `asks.get` / `asks.answer` | by id; answer carries the exact offered `optionId`, or question answers, or a plan outcome. First **valid** answer wins; an invalid one is `bad_request` and leaves the ask open |
| `session.set` | model, mode, config, title; applied in the engine's order, answered and broadcast with the confirmed value and a revision |
| `session.stop` | explicitly end the session and its host. Distinct from closing a connection or a view (SD-28); defined in S2 |
| `session.create` | hub only (S4): spawn a headless host |

Notifications carry the subscription id they belong to: `event` (`seq` + the
event in the lossless codec), `reset{reason}`, `synchronized`; and
`roster` (epoch + cursor) for roster subscriptions. Ask transitions are
ordinary sequenced events.

## Attach and resume (SD-09, SD-18)

1. Client sends `session.attach` with its cursor, if any. A cursor is
   `{incarnation, seq}`; one from another incarnation is a `reset`.
2. Under the engine's ordering boundary the host registers the subscription
   and reads the current `seq` N: that is the **single cutoff**. Live delivery
   starts at N+1, so nothing emitted during the read can be lost.
3. `(cursor.seq, N]` is replayed from the in-memory ring and, for anything
   older, from the journal file up to its complete-record boundary. The
   decision to replay is bounded by **bytes as well as rows**; otherwise, or
   when the range crosses a journal gap, the host sends a bounded snapshot
   stamped N (from S1c) instead.
4. Then `synchronized`.
5. A cursor the host cannot honor gets an explicit `reset{reason}`
   (`cursor_unresolvable`, `journal_gap`, `slow_consumer`), never a silent gap.
   A session whose `session/load` failed never reaches `synchronized` and
   accepts no mutating command.

Replay and live use the same per-event representation; nothing is coalesced
in the log (SD-18). Unlike gx, **ask transitions are sequenced events in the
same log**, so they resume exactly; gx lists "an API-owned journal" as future
work and shed papers over its absence with a re-fetch and tombstones on every
reconnect. The roster is ordered separately, by its own epoch and cursor.

## The verb × activity gate

Published in the spec as a table, enforced by the engine (`03`):

| activity | `queue` | `interject` | `send_now` | `cancel` |
|---|---|---|---|---|
| starting / replaying | refuse | refuse | refuse | refuse |
| working | allow | allow if the provider can | allow | allow |
| blocked on an ask | allow | `not_accepting` | `not_accepting` | allow |
| foreign turn (the agent's own) | allow | `not_accepting` | `foreign_turn` | allow, written at once |
| idle | allow | `not_accepting` | allow | `not_accepting` |
| idle **with an ask pending** | allow | `not_accepting` | allow | **allow** |
| failed / cancelling / closing | to be specified in S2 | | | |

Asks arrive between turns and during foreign turns too, so cancel is accepted
whenever any ask is pending, whatever the activity. The table is a sketch; S2
derives it from the code's actual states rather than from these three words.

Provider limits come from the session's capabilities, so a client hides what
a provider cannot do instead of discovering it by error.

Queue edits carry `expectedVersion` (`QueuedPrompt.Version` already exists),
so two clients editing one row cannot silently overwrite each other.

## Errors

`{code, message}` with a closed set of string codes that map onto shed's
`LaneError`: `bad_request`, `unknown_session`, `unknown_ask`,
`already_submitted`, `already_resolved`, `not_accepting`, `unsupported`,
`unavailable`. The refusals craze already distinguishes keep their own codes,
because a client needs them to keep the user's draft: `queue_full`,
`text_too_long`, `prompt_in_flight`, `foreign_turn`, `prompt_cancelled`,
`stale_version`, `stale_turn`. No client should ever match on message text.

## One session per connection (SQ14)

A hub that multiplexes N session streams on one client connection has to
rewrite request ids, merge notifications, and own per-client budgets and
`reset`, because the fast host→hub hop hides the slow phone from the host.
That is session semantics in the hub, and a hub that parses methods must route
ones it does not know, which weakens the one-protocol-integer argument (`02`).

The default is therefore: a connection carries the roster **or** one attached
session. On `session.attach` the hub splices bytes to a dedicated host
connection, so kernel backpressure reaches the host's own budget logic.
`sessionId` sits at a fixed top-level position in params so unknown methods
still route, and `hello` reserves a hub-only provenance field so a host can
later tell a relayed client from a local one (S7). Subscription ids stay, so a
later multiplexing transport is not precluded. Extra connections are cheap for
SSH clients: shed-mobile's stable local port already opens one bridge exec per
accepted connection.

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
when one is running, otherwise the host's socket in the runtime namespace
(`02`). SSH clients run it as an exec command, the way shed-mobile already
runs `roost-session client-bridge` for roost. It composes nothing from its
input and prints nothing but protocol bytes on stdout.

S2 must specify what an SSH exec cannot assume (SD-27): how the bridge finds
the same runtime namespace without the launching tab's environment, how the
client finds the `craze` binary (roost's `remote_command_for` is the model),
half-close and teardown behavior, that `--session` is validated as an opaque
id before any path is built, and that the bridge verifies the peer uid of the
socket it dials.

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
