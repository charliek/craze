# 03 — Engine core (S1)

The work every later phase depends on. It ships no feature: the exit bar is
that the TUI behaves exactly as before while being the **first client** of an
engine that could serve a second one.

## What exists (verified 2026-09-19)

| asset | where | why it matters |
|---|---|---|
| `agent.Session`: a complete lifecycle interface with no TUI imports | `internal/agent/session.go:424` | It is nearly the RPC surface already |
| Three implementations behind it: ACP, native, test stub | `internal/agent/live.go`, `native.go` | The engine work is provider-neutral by construction |
| No singletons in `internal/agent` or `internal/acp` | `agent.New`, `live.go:212` | N sessions per process is possible; one per process is a choice |
| A headless driver of the full lifecycle | `internal/cli/prompt.go` | The shape of a host without a TUI |
| Every event kind already serialized to JSON | `internal/cli/events.go:14`, `:176`, `:189` | The starting point for the wire schema |
| A non-blocking bounded multi-consumer fan-out | `internal/host/hub.go:80` (`queueCap = 8`, run collapsing, panic isolation, bounded close) | The template for the event broker; today it carries only `host.Status` |
| Status derivation with a fixed priority | `internal/host/status.go` (`Derive`) | The roster's activity field should come from the same function |

## What is missing

### 1. Event fan-out

`Events()` returns one `chan Event` of capacity 256 (`live.go:222`,
`native.go:106`); the TUI's `waitEvent` (`internal/tui/app.go:2477`) is a
consuming reader, so a second reader would **steal** events. `emitCtx` blocks
when the channel is full, so a slow consumer can stall the agent's read loop.

Needed: one broker per session that reads the emit path once and feeds N
subscribers, each with a bounded queue and a byte budget. A subscriber that
exceeds its budget is **closed with a reason and re-syncs** (t3code's
`LiveStreamBudget`, gx's `reset{slow_consumer}`); it never slows the agent or
other subscribers. The journal (`04`) is the one subscriber that may apply
backpressure, and only briefly.

Mind the documented lock order in `live.go` (`queueOp → emitMu → s.mu`): the
comment exists because emit has wedged before.

### 2. A sequenced, engine-owned transcript

The only structured history is `internal/tui/transcript.go` (`entry` at `:59`,
`transcript` at `:87`), fused with a lipgloss render cache. For ACP providers
the engine keeps a `Snapshot` but no message history at all; a fresh client
would see only the live tail.

Needed:

- Every emitted event gets a **monotonic per-session sequence number**, which
  is also its journal position (`04`).
- A **render-free transcript model** in the engine, fed by the same events:
  the `entry` data (kind, text, tool, plan, timestamps, open/closed, interject)
  without `rendered` / `renderedFor` / `dirty`. The streaming-run merge, tool
  upsert, and cap/trim behavior move with it. Entries get **stable ids** so
  updates are upserts, which is what makes resume inside a streaming run safe
  (SQ2).
- The TUI keeps a render cache keyed by entry id and nothing else.

This is the riskiest item in the roadmap: about 580 lines of
`transcript_test.go` and about 1,600 lines of frame goldens are pinned to
today's rendering. The refactor must be behavior-preserving and land with the
goldens unchanged.

### 3. Engine-owned asks and turn state

Two clients must agree on "which ask is open" and "is this turn over". Today
that is `tui.Model` state: `cards` / `cardsCancelled`, `turnSeq` /
`promptEndSeq` / `streamEndSeq`, `turnSettled` (`app.go:1846`), plan-offer
arming, `modeInFlight` / `modeGen`, and the policy for writing
`sessions.jsonl`. The session already parks requests in
`waiting map[string]pendingAsk` (`live.go:47`), but only the TUI sequences
them.

Needed (SD-10):

- An **ask registry** in the engine: each permission, question, or plan ask is
  a resource with an id, kind, status (`pending → submitted → resolved`), the
  options as offered (ACP option kinds, opaque option ids), timestamps, and
  who answered. **First answer wins**; every subscriber sees the resolution;
  a late answer gets `already_resolved`. Readable by id, which is the single
  biggest cost difference between shed's two existing adapters (`06`).
- **Turn state** published by the engine: settled, stop reason, blocked-on
  count. `host.Derive` takes it from the engine, not from `tui.Model`.
- **Cancel is acknowledged**: a command with a result, not fire-and-forget.
  gx's unacknowledged `session/cancel` is a documented dead end on the phone.
- Session-index writes (`sessions.jsonl`) move to the engine so a headless
  host indexes itself. Today `craze prompt` writes no row.

### 4. Commands

Every mutating call carries a **client-generated command id** (SD-11). The
engine keeps a bounded in-memory receipt table so a resend after a dropped
connection returns the original outcome instead of sending the prompt twice.
t3code persists receipts in SQLite; in-memory is enough while a host's life
bounds a session's live state. The journal records each command and its
outcome (`04`).

The gate for each verb is the session's state, published as a table (`05`):
for example `interject` is refused with `not_accepting` unless a turn is
working, and `cancel` on an idle session is refused rather than swallowed.

## Shape of the change

Suggested split, each independently mergeable and gated:

| PR | content | risk |
|---|---|---|
| S1a | Broker + `Subscribe`; sequence numbers; journal writer as a subscriber; TUI moves from `Events()` to a subscription | low; touches the emit path in `live.go` and `native.go` |
| S1b | Ask registry, turn state, acknowledged cancel, command ids, index writes in the engine; TUI cards read from the registry | medium; subtle ordering code leaves `app.go` |
| S1c | Render-free transcript model in the engine; TUI keeps only a render cache; snapshot + `afterSeq` attach implemented in-process | high; goldens must not move |

`craze prompt --json` should gain `seq` on every line in S1a: a cheap,
observable proof the numbering is right, and H6's child processes will want it
(`11`).
