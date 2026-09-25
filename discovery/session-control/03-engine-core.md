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

The driver also **publishes activity**. `host.Hub.Publish` is driven by the
TUI's transitions today, and a headless host has no TUI: the engine feeds the
status stream that `host.Derive` consumes, and the roster's activity field
comes from it. The roost and herdr reporters stay wired only where there is a
tab to report to (SD-15).

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

**Fold cost is a plan gate.** The fold runs under the one lock every emit
path shares, where today the equivalent work happens on consumer goroutines.
It must be amortized O(1) per event (builder-style text append, pointer-swap
tool state, never a re-merge of an entry's whole text), and S1c's convergence
check also asserts lock-hold bounds.

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

## S1b, as shipped (2026-09-21)

The sections above describe the design as planned, from before S1a existed.
`internal/engine.Engine` is now the code; this is how it actually locks and
what its three workers do, so a later phase reads the real thing rather than
the proposal (Plan 021 §3.1–§3.8, its execution amendments, and the package
doc comments on `engine.Engine`, `receiptTable` and the index worker).

**Lock order.** `e.mu → s.mu`: the engine calls exactly three things on the
session while holding `e.mu`, all leaf on `s.mu` and none waiting on anything —
`Begin`, the accessor `ForeignTurn()`, and, for a session that has one, the
admission fence's `FenceUp`/`FenceDown` (`agent.AdmissionFence`, Plan 026 PR
3): a session that can start a turn of its own (native's wake) refuses to while
it is up, and the engine keeps it up whenever it is not idle — raised before
every read of the flag, every `Begin` and every cancel's validation, and
brought back to what the engine is at the end of each such section.
`e.mu → the outbox mutex`: every engine-authored event is `Enqueue`d in the
locked section that made the change it describes. `registry.mu → the outbox mutex`, the same
shape one layer down for the ask registry. **`s.mu → the outbox mutex`**:
every settings delta is enqueued by the *session*, under `s.mu`, in the
section that mutates its own snapshot (§3.8) — the engine does not author
those; it only serialises the calls that cause them (the settings worker,
below). **`s.mu` is never held across a registry call, for the live session
and the Stub; native is the one exception, and holds it across the
non-blocking `CancelTurn` only**, stated in `native.go`'s own struct comment
(Plan 023 §3.5, the harness's use of the registry through the adapter). The
receipts table's mutex and the index worker's mutex are each a leaf of their
own: the receipts table's is held only to admit and to finish a command,
never across one — C12's index I/O inside `Submit`/`SetTitle` would otherwise
block every client's commands (PR 3's r24 finding). `e.mu` and `registry.mu`
are never held across a blocking call — a provider call, `Session.Cancel`, a
continuation, `Publish`, `Flush`, file I/O — and never nested in each other.

**The workers.** Three, each engine-owned and joined by `Close`:

- **The driver**, one per engine: `for { <-kick; try to settle, else try to
  drain }` on a one-slot channel. Every source of a wake-up kicks it — the
  log's observer (a foreign turn ended, a replay ended), a returned
  continuation, a released cancel hold — and every kick re-reads state under
  `e.mu`, even when the last pass did nothing, so two collapsed kicks are
  harmless and a missing re-check would be a queue that never drains with no
  error anywhere.
- **The settings worker**, one FIFO: `Control.Set` calls queue behind it, and
  each runs provider call → the session's locked mutate-and-enqueue → `Flush`
  → return `SetResult{Value, Rev}`, where `Rev` is read back from
  `EventLog.EnqueueTicket` (the drainer's own record of the batch's first
  committed `Seq`). Plan 025 added the worker's `Setting.ForModel` check: a
  change bound to a model the session has since left is refused
  `ErrStaleModel` before the provider is asked — atomic, because the FIFO
  serialises every `Set`, and refused again, the same way, under `s.mu` in
  the session's own section just before the write, for a model move landing
  in the instant between (`stale_model`, `05`). And what a settings reply
  says the agent now holds — `set_config_option`'s catalog, `set_model`'s
  model — is never written by the setter's own locked section any more: the
  **read loop** installs it, under `s.mu`, before the call returns to its
  caller, ordering a reply against the agent's own pushes by wire arrival
  rather than by which goroutine takes `s.mu` first; the setter's locked
  section only announces — its one delta is built from the snapshot as that
  section finds it (the reply's install plus whatever the agent pushed after
  it), and its `SetOutcome`/`SetResult` value is read from the same place
  (plan 025 designs 1–3). The lock-order paragraph above still holds: the
  read loop's install and the setter's announce are each their own `s.mu`
  section, `s.mu → the outbox mutex` either way, never nested.
- **The index worker**, fed by the observer through a one-slot, latest-wins
  channel: touch, load and title writes merge under its own leaf mutex, and
  `Close` gives it one 500 ms bound (the journal's own close bound) to attempt
  a last write before abandoning it, because `flock` cannot be interrupted.

**What `Close` does, in order.** Under `e.mu`: mark the engine closing; if a
turn is current, end it there — synthetic, stop reason `closing`, the
queue's length as `Pending` — because this is the only place left that can
say so, and it is a mandatory completion like every other ending; disarm any
armed send-now, carrying the same cause; `Enqueue` that one batch. Release
`e.mu`. Then, outside any lock: `sess.Close()` — which resolves every parked
ask (`closing` for an ACP session and the Stub; native cancels its turn context
first, so an ask parked there commonly ends `cancelled`, by `call`, and
`closing` only when the registry's close wins that race — Plan 023 §3.5, and
`05`) and closes the log last, so what the outbox still holds is
committed to the ring, the journal and every subscription with a
**non-blocking** primary send, never lost to a stopped reader; `close(e.done)`;
`e.wg.Wait()`, joining the driver and the settings worker; `e.idx.close()`,
the index worker's own bounded last attempt. A second `Close` is a no-op
(`sync.Once`) and returns the first call's error.

## S1c, as shipped (2026-09-24)

§8 above describes the transcript model as planned, before any of it existed.
`internal/transcript` is now the code (Plan 024, two PRs: `feature/plan-024-s1c-model`
#50 `27c1db6`, `feature/plan-024-s1c-tui` #52, merged 2026-09-24); this is
what changed from
the design as it went in, so a later phase reads the real thing rather than
the proposal (the plan's §3, its execution amendments, `12` S1c).

Two instances exist, both running the same `Fold` code. The **engine's** is
folded as the first statement of `engine.observe`, ahead of the sub-agent
guard, under a leaf `model.mu` taken **inside** the log's publishing boundary
(lock order: the boundary → `model.mu`); `Snapshot` takes the same `model.mu`
**outside** the boundary, so the two are never nested in the other
direction. It is the authority a snapshot is cut from. **Every client folds
its own** from the events it
receives — the TUI from its primary, on its own goroutine — because SD-33
already says the TUI is a socket client for good and must not start by
reading the engine's memory. Entries are immutable (every mutation a new
`*Entry`), addressed by `EntryID{Seq, N}`, and bounded (§3.2 (b)'s numbers,
`10` SQ9). `Engine.Attach` (snapshot + cursor → live events from N+1) joined
`Control` in process; S2 wraps it for the wire (`05`).

Five places where the shipped design departs from the prose above, all
recorded in the plan's §4 and mirrored into `12` by C9:

- **A command's transcript effect is the in-process client's own row with the
  echo hidden**, not "the engine applies a command's transcript effect
  inside the command call and returns the entry and its `seq`": S1b's echo
  rule already draws the user's own row synchronously on `Submit`, so PR 2
  makes the pane skip the shared entry the fold creates for it (the started
  of `ownTurn`) rather than routing the row through the fold at all.
- **"The TUI keeps a render cache keyed by entry id" holds**, unchanged.
- **Client-local entries live in the pane's display list, where today's
  slice put them**, not in "a TUI overlay anchored after an engine entry
  id": an anchored overlay would reorder rows relative to today's single
  slice, which the goldens do not tolerate. The pane is `rows []rowRef`
  (each a shared `EntryID` or a local row), maintained incrementally from
  the fold's `Change` plus the same local-row call sites §2.4 named.
- **The event's `At` stamps every shared row; a client's clock is a fallback
  for a zero stamp**, not "the engine takes the injected clock: entry
  timestamps and thought-run elapsed time come from the TUI's `m.now()`". A
  run's `End` is the `At` of the event that closed it. The engine's instance
  passes no clock at all (a zero `At` stays zero, since no callback may run
  under the boundary); the TUI's passes `m.now`, used only when an event
  arrives unstamped.
- **`07`'s "folded inside the boundary" holds for the engine's instance;
  clients fold their own.** Nothing folds on a pump or a subscription
  goroutine of the engine's; every client's fold runs on that client's own
  goroutine, from whatever the client's own subscription (in process: the
  primary) delivers it.

## Shape of the change

| PR | content | risk |
|---|---|---|
| S1a | Ordering boundary + `Subscribe` with primary and budgeted subscribers; sequence numbers; incarnation identity; the lossless codec; the ring; the journal writer as a budgeted subscriber; `seq` on `craze prompt --json`. `Events()` keeps its meaning as the primary subscription | medium: correctness-sensitive concurrency on the emit path, but no behavior change |
| S1b | The engine turn driver; ask registry; cancel outcomes; command ids; settings order; approval policy; durable craze session id and index writes in the engine | high: the subtlest code in `app.go` leaves it |
| S1c | Render-free transcript model folded in the boundary; bounded snapshots; in-process attach with snapshot + `afterSeq`; convergence check per event kind | high: goldens must not move |

Index writes moving into the engine (S1b) must keep native sessions out of
`sessions.jsonl` until the harness has a loader (H7): attachable live sessions
and resumable stored sessions are different lists (`11`).
