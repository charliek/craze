# 03 — Engine core (S1)

The work every later phase depends on. It ships no feature: the exit bar is
that the TUI behaves exactly as before while being the **first client** of an
engine that could serve a second one.

Revised 2026-09-19 after the panel review and the S1a planning read (`12`);
the rulings are SD-18 to SD-26.

## What exists (verified 2026-09-19)

| asset | where | why it matters |
|---|---|---|
| `agent.Session`: a complete lifecycle interface with no TUI imports | `internal/agent/session.go` (`Session`) | It is nearly the RPC surface already |
| Three implementations behind it: ACP, native, test stub | `internal/agent/live.go`, `native.go`, `internal/tui/stub.go` | The engine work is provider-neutral by construction |
| No singletons in `internal/agent` or `internal/acp` | `agent.New` | N sessions per process is possible; one per process is a choice |
| A headless driver of the full lifecycle | `internal/cli/prompt.go` | The shape of a host without a TUI, **but** it is a second turn driver and it auto-answers asks (below) |
| A JSON projection of events for `craze prompt --json` | `internal/cli/events.go` | A stable CLI contract. **It is lossy** and is not the wire or journal schema (SD-20) |
| A worker-per-consumer fan-out | `internal/host/hub.go` | Only its shape carries over (a worker per subscriber, panic isolation, bounded close). It is a **lossy last-value** fan-out: it drops identical statuses, debounces idle, and sheds the least informative entries from a queue of 8. A sequenced log must not |
| Status derivation with a fixed priority | `internal/host/status.go` (`Derive`) | The roster's activity field should come from the same function |

`internal/cli/events.go` drops permission ids and option names and kinds,
question bodies, plan bodies and todos, `Replayed`, timestamps, tool raw input,
locations and diff text, caps tool output at 2 KiB, and discards bare
`EventMeta` (which native uses to announce a model change). Nothing can be
replayed from it.

Naming: `internal/host` and `host.Hub` already mean the herdr and roost
status integration. The session host and the hub of `02` need package and
type names that do not collide with them.

## What is missing

### 0. The event stream is not yet a complete record (SD-30)

Much of what the transcript shows never passes through `agent.Event`:

- **The user's own prompt** is written by the TUI (`sendText` → `addUser`);
  the session deliberately drops the agent's live echo in `onUpdate`, because
  emitting it would double user blocks and add lines to `craze prompt --json`.
- **Ask outcomes** ("? … → label", "→ skipped", "plan … →"), mode notes,
  "renamed to", the cancelled note, restored and foreign-turn notes, and
  errors from the `Prompt` return path are all TUI-authored rows.
- **Session state is pull-only.** `EventMeta` is usually empty and means "call
  `Snapshot()`"; `SetTitle` emits nothing by contract. A socket client has
  nothing to pull, and "settings are broadcast" has no event to broadcast.

So before anything can replay to the same transcript, S1b inventories every
`addUser` / `addNote` / `addError` / `addPlan` call site and splits them into
**shared** (the engine emits them: prompt accepted with its text, ask resolved
with its outcome, mode / model / title changed with the value, turn settled,
session-level errors) and **client-local** (they stay in the TUI: theme notes,
usage errors). Bare `EventMeta` is replaced by state deltas, and the attach
snapshot carries a sequenced session-state document. `craze prompt --json`
is a filtered projection, so the new kinds do not change its output.

S1a does not wait for that: it journals each prompt's text as a journal-only
line from the session's own `Begin`/`Prompt` path, so the record is useful
from the first day (`04`).

### 1. One ordering boundary, then fan-out (SD-18, SD-19)

`Events()` returns one `chan Event` of capacity 256; the TUI's `waitEvent` and
`craze prompt`'s loops are consuming readers, so a second reader would
**steal** events. Emits come from several goroutines and only queue
transactions serialize them (`emitMu`).

Two facts constrain the design:

- **Callers rely on "emitted means buffered".** `craze prompt` drains
  `Events()` non-blockingly after `Prompt` returns and expects everything the
  turn emitted to be there (`drainBuffered`, `runTurn`); the TUI's
  `turnSettled` races the same two endings. A broker goroutine between emit and
  the consumer would break that. Fan-out therefore happens **inline in emit**.
- **`queueTx` calls emit while holding `emitMu`**, and the documented lock
  order is `queueOp → emitMu → s.mu`, with `s.mu` never held across an emit.
  The boundary is a **new leaf lock**; it never calls back into the session.

Under that one lock, per event: assign the next sequence number, fold it into
the engine's transcript model (from S1c), append it to an in-memory ring,
enqueue it to every subscriber. No I/O and no session calls under the lock;
event payloads are immutable once emitted (deep-copied where the session keeps
mutating the original).

Subscribers are of two kinds:

| kind | policy | who |
|---|---|---|
| **primary** | blocking, lossless, exactly today's `Events()` semantics, including the `s.done` escape | the in-process TUI or `craze prompt`; at most one |
| **budgeted** | bounded by items and bytes; on overflow it is closed with `slow_consumer` and must re-attach | everything else: the journal writer, sockets, tests |

The boundary is **one provider-neutral component used by every `Session`
implementation**, including `tui.Stub`, which drives every golden: a fan-out
built only into `live.go` and `native.go` would be bypassed by the goldens. A
wrapper that is the sole reader of `Events()` was considered and set aside
for S1a: it puts a pump between emit and consumer, so every mutating method
would need a delivery barrier to keep "emitted means buffered". S1b's driver
and registry may still be a layer above the session.

Events emitted after `done` are dropped today and `events` is never closed,
so final events can be lost at shutdown; S1a defines a bounded shutdown drain
for budgeted subscribers, the journal first.

**Sequence numbers order events, not state.** `Snapshot()` reads provider
state independently and can already include a mutation whose event is not yet
numbered. Two rules make attach correct anyway: the transcript model is folded
*inside* the boundary, so its snapshot at `seq` N is exact; and every
state-carrying event (queue, tools, sub-agents, todos, settings, asks) must be
**convergent under re-application**, which S1c verifies kind by kind.

### 2. The event log is per event, uncoalesced (SD-18)

Live subscribers and replay see the **same representation**: every emitted
event, one sequence number each. Coalescing deltas in the log (the original
SQ2 default) was dropped: a client attaching between an emitted delta and its
flush would find it in neither the journal nor its live buffer, and stable
entry ids prevent duplicates but do not recover missing updates.

Attach cutoff: under the boundary, register the subscriber and read the
current `seq` N. Replay `(afterSeq, N]` comes from the ring, and from the
journal file for anything older than the ring; live delivery starts at N+1.
The ring is what covers the journal writer's unflushed window. If neither can
serve the range (ring overrun **and** a journal gap), the answer is `reset`
and, from S1c, a snapshot.

Folding deltas into entries is the **transcript model's** job (S1c), and
snapshots are what bound replay cost, not a lossy log.

### 3. A lossless, versioned event codec (SD-20)

A new codec for `agent.Event` ⇄ JSON with a round-trip test over every event
kind and field, `Err` carried as text plus a class, timestamps preserved. It is
the journal's `event` payload now and the wire's later. `craze prompt --json`
stays a projection with its own compatibility promise and gains only `seq`.

### 4. Identity (SD-22)

Three different things, never conflated: the **durable craze session id**
(survives `session/load` into a new agent session and a host restart), the
**provider session id** (what roost ownership and `sessions.jsonl` key on
today), and the **host incarnation** (one process lifetime). Sequence numbers
are scoped to an incarnation: a cursor names its incarnation, and a cursor
from another incarnation is a `reset`, never a guess. That also removes the
"observed but unpersisted tail reuses numbers after restart" hazard.

S1a mints incarnations and records provider ids; the durable craze id arrives
with engine-owned index writes in S1b.

### 5. The engine owns the turn driver, not just turn state (SD-24)

Today `tui.Model` is the driver: `sendText` writes the user row and claims the
prompt, `drainSettledTurn` pops the queue and starts the next turn,
`promptDoneMsg` supplies the ending for refusals and pre-wire cancels that emit
no terminal event, send-now arbitration lives beside them; `craze prompt`'s
`runLoop` is a second driver. `Session.PopQueue` is explicitly only a guard.
Moving counters and cards alone would leave two clients able to drain one
queue, or a headless queue that nobody drains.

S1b gives the engine one driver: prompt admission, queue removal-plus-claim,
send-now arbitration, prompt completion including **synthetic endings and
synthetic transcript events** (user rows, notes that belong to the session
rather than to one client's view). Clients submit commands and observe
results. Goldens are preserved through that canonical projection, including
the injected clock.

### 6. Asks (SD-25, SD-26)

An engine registry: each permission, question, or plan ask is a resource with
an id, kind, the options **exactly as offered**, status
(`pending → submitted → resolved`), timestamps, and who answered.

- **First *valid* answer wins**: validation and claim are one atomic step.
  Today `AnswerPermission` removes the request before validating the option
  id, so an invalid answer cancels it.
- Offered options are preserved verbatim, **including two options of the same
  kind**: grok really offers `enable-always-approve` with kind `allow_once`
  (`OptionIDForKind` exists for that). Semantic shortcuts must reject
  ambiguity; the exact offered id is always accepted.
- Opening and resolution are ordered against cancel (`emitParked` releases its
  lock before publishing; the TUI's cancellation state masks the race today).
- Engine acceptance is distinguished from provider delivery, and **every
  ending is a sequenced terminal event**. Today most are silent: `cancelWaiting`
  answers every ask, `emitParked` drops one whose turn died, `awaitDecision`
  falls through on close, `Force` auto-allows, non-interactive sessions
  auto-answer, a bad option id answers "cancelled", and the TUI just nils its
  cards. The outcomes are: answered, cancelled by cancel, turn ended, host
  closing, automatic. Terminal records are kept, bounded.
- Ask ids are per-process counters today (`perm-N`, `ask-N`, `plan-N`) and
  collide across restarts; they become unique per incarnation.
- **Approval policy is separate from frontend presence.** `Options.Interactive`
  false auto-answers questions and accepts plans, and `craze prompt` defaults
  to force. A headless host must be able to park asks with zero clients and
  keep them across disconnects; `craze prompt`'s behavior stays, as an
  explicit policy that goes through the same registry and journal.

### 7. Cancel, commands, settings (SD-25)

- **Cancel** answers with an outcome against an **engine turn id**: rejected,
  requested, or settled, with an honest "unknown" for a timeout after the
  write. The existing wire outcomes (pending / sent / refused / failed /
  withdrawn, plan 017) and the cancel-before-prompt ordering are preserved. A
  client may condition a cancel on the turn it displayed, so a cancel delayed
  across a queue transition cannot hit the next turn.
- **Command ids** are reserved atomically while in flight (a duplicate waits
  for the first), matched on payload, scoped to the incarnation, and kept for
  an **advertised retry horizon**; outside it the answer is "unknown", never
  a silent re-execution promise. JSON-RPC request ids are not command ids.
  Losing the connection does not cancel an admitted command.
- **Settings** (model, mode, config, title) get an engine-controlled order,
  serialized provider calls, and a confirmed resulting value with a revision,
  published as an event. Today the three setters call the provider outside
  `s.mu` and complete in any order, and `SetTitle` emits nothing; moving
  `modeGen` out of the TUI does not make two clients converge.

### 8. The transcript model and snapshots (S1c)

The only structured history is `internal/tui/transcript.go`, fused with a
render cache. The render-free model moves into the engine, folded inside the
ordering boundary, with stable entry ids and revisions; the TUI keeps a render
cache keyed by entry id.

Bounds are part of the design, not an afterthought: the main transcript has
no text budget today, and 5,000 entries at the 64 KiB stream cap approach
313 MiB before encoding. S1c sets aggregate byte bounds for the model, for a
snapshot, and for replay working memory (bounded decoding, never whole-file
loads), chosen so a snapshot can succeed inside a subscriber's budget.

This is still the riskiest item: about 580 lines of `transcript_test.go` and
about 1,600 lines of frame goldens are pinned to today's rendering. "Goldens
must not move" holds only under three conditions:

- **Client-local entries need a home.** Theme notes and usage errors
  interleave with shared entries in one ordered slice today. They live in a
  TUI overlay anchored after an engine entry id.
- **A command's effect stays synchronous for the in-process client.** The user
  block is drawn in the same `Update` as Enter. Routing it through an
  asynchronous subscription adds a frame, which frame captures see; echoing
  locally as well brings back the documented double block. So for the
  in-process client the engine applies a command's transcript effect inside
  the command call and returns the entry and its `seq`.
- **The engine takes the injected clock**: entry timestamps and thought-run
  elapsed time come from the TUI's `m.now()` today.

The exit wording is therefore "golden files byte-identical;
`transcript_test.go` assertions move packages unchanged".

Tool events re-emit the full merged tool state on every update, diff text and
an 8 KiB output tail included, so a tool that streams output grows the log
with the square of its update count. Full-state events are convergent, so
superseded ones may be conflated in a queue without harm; whether to do it is
measured first (SQ15). A linear log also cannot express a **rewind** of the
native store's tree: a `transcript.reset{fromEntryId}` event kind is reserved
for it now (`11`).

## Shape of the change

| PR | content | risk |
|---|---|---|
| S1a | Ordering boundary + `Subscribe` with primary and budgeted subscribers; sequence numbers; incarnation identity; the lossless codec; the ring; the journal writer as a budgeted subscriber; `seq` on `craze prompt --json`. `Events()` keeps its meaning as the primary subscription | medium: correctness-sensitive concurrency on the emit path, but no behavior change |
| S1b | The engine turn driver; ask registry; cancel outcomes; command ids; settings order; approval policy; durable craze session id and index writes in the engine | high: the subtlest code in `app.go` leaves it |
| S1c | Render-free transcript model folded in the boundary; bounded snapshots; in-process attach with snapshot + `afterSeq`; convergence check per event kind | high: goldens must not move |

Index writes moving into the engine (S1b) must keep native sessions out of
`sessions.jsonl` until the harness has a loader (H7): attachable live sessions
and resumable stored sessions are different lists (`11`).
