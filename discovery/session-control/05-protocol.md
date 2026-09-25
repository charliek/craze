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
JSON-RPC request id. As shipped, a `commandId` must be a canonical positive
base-10 integer — no sign, no leading zero, no whitespace; anything else is
`bad_request` — numbered per client from 1, and the client id it travels with
must be one the engine minted (S2: minted per connection and bound to it; an
unminted client id is `bad_request` too). Capabilities come at two levels
(SD-28): the connection's (`hello`) and each session's (its roster row and
attach reply), because a hub fronts hosts with different providers and
versions.

Event payloads use the **lossless event codec** from S1a (`03`, SD-20), not
`craze prompt --json`'s shapes, which drop ask ids, option kinds, plan bodies,
and more.

| method | notes |
|---|---|
| `hello` | protocol version (one integer), client kind, client capabilities; reply carries the endpoint's identity (host or hub), craze version, connection-level capabilities, the command retry horizon |
| `sessions.list` / `sessions.subscribe` | roster rows: id, title, cwd, provider, model, activity, **pending ask count**, parent id, last change, host incarnation, session capabilities. Readable without attaching. The roster has its **own epoch and cursor**; a new epoch means reseed |
| `session.attach` | `{cursor?: {incarnation, seq}}` → a **subscription id**, then `snapshot` (or replay), events, `synchronized`. `session.detach{subscriptionId}` ends it; closing the connection ends all of them and **never** stops the session |
| `session.prompt` | `{text, mode: queue \| interject \| send_now}`, admitted by the engine's turn driver. As shipped, `queue` and `send_now` go through `Control.Submit(mode: queue \| send_now)`, answered with exactly one of the turn id it started, the queued row (no turn id yet — nothing to be "queued behind"), or `armed: true`; `interject` goes through a separate blocking `Control.Interject`, which answers with only an error. S2 must map the wire's three modes onto those two calls |
| `session.cancel` | `{turnId?}` request/response: `rejected` (`not_accepting`, or the named turn is no longer current), `requested`, `settled`, or `unknown` after a timeout past the write |
| `session.queue.*` | edit, remove, clear; mirrors `engine.Control`'s queue verbs (the queue left `agent.Session` in S1b) |
| `asks.list` / `asks.get` / `asks.answer` | by id; answer carries the exact offered `optionId`, or question answers, or a plan outcome. First **valid** answer wins; an invalid one is `bad_request` and leaves the ask open. `asks.get` is `Control.Ask(id)`, waiting on nothing. An id whose opening was never published has the hidden spelling `perm-xN` / `ask-xN` / `plan-xN`, from a counter of its own, so it can never renumber a visible one: a refused open, a request the provider answered before any handler ran, an automatic PERMISSION resolution, and an automatic question/plan fallback that cannot publish (a bad or cancelling automatic answer). An automatic question/plan answer that itself publishes its `Auto` opening keeps a visible id |
| `session.set` | model, mode, config go through `Control.Set` and are applied in the engine's order, answered and broadcast with the confirmed value and a revision: `{value, rev}`, where `rev` is the delta's own `seq` — the same number the broadcast `event` notification carries — and may be `0` when no committed revision is available: the change was made, but its delta could not be confirmed committed (a flush that gave up) or was never committed at all (a `Set` that lost the race to the session closing), so a client must not wait for a matching event. Title is a separate call, `Control.SetTitle`: it waits on nothing, bypasses the settings worker, and returns only an error — its delta, not its reply, is how a client learns the title |
| `session.stop` | explicitly end the session and its host. Distinct from closing a connection or a view (SD-28); defined in S2 |
| `session.create` | hub only (S4): spawn a headless host |

Notifications carry the subscription id they belong to: `event` (`seq` + the
event in the lossless codec), `reset{reason}`, `synchronized`; and
`roster` (epoch + cursor) for roster subscriptions. Ask transitions are
ordinary sequenced events.

A parked ask's `closing` outcome is **guaranteed** when the host session
closes, for an ACP-backed session and the Stub: both resolve every open ask
before their event log closes. It is **best-effort** for a native session's
own asks (Plan 023 §3.5): native cancels the turn's context first, so a
parked native ask that context reaches ends `cancelled, by: "call"` rather
than by an explicit `closing` resolution. And a turn current when the session
closes always gets its `ended`: the host publishes it itself, synthetic, stop
reason `closing`, in the same section that starts shutdown, before the
session (and so the log) closes — never left `started` with no ending on the
record (session control S1b's own live-smoke finding, `12`).

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

**Attach during a `session/load` replay.** The host's socket exists from
process start and replayed events are ordinary sequenced events, so a large
replayed history crossing a budgeted subscriber as live delivery could trip
`slow_consumer` and loop. An attach that arrives while a load replay is in
progress is answered after `EventReplay{end}`, or served as a snapshot at the
cutoff.

**In process, this is `Engine.Attach` (S1c) minus the socket.** `Control.Attach`
does the same two steps as above without a wire: a cursor if the log can
serve it, else `Model.Snapshot(budget)` (bounded, lossless, `10` SQ9) taken
under `model.mu` outside the boundary, then `Subscribe(After: {inc,
snap.Seq})`. Its three error paths, pinned by the plan (`12` S1c "Deviations",
X21–X22): a **synchronous refusal** of the cursor — the snapshot's own or the
client's — is retried with a fresh snapshot (first attempt + 3 retries), then
`ErrAttachRaced` (`unavailable`, nothing stored, ask again); an
**asynchronous** failure on the subscription's own leg (`evicted`,
`journal_behind`, a read error, possibly after a prefix was delivered) is
answered by the client discarding everything since the snapshot and
re-attaching with no cursor; and an **`Omitted` record** (a body over
`MaxRecordBytes`, folded by the observer but undecodable by any subscriber)
is the same — re-attach with no cursor, which lands at or beyond the omitted
seq, needing at most two re-attaches because of `commitLocked`'s own commit
order (X22). An attach during a replay is served at the cutoff and folds
through `EventReplay{end}` like any other event, no special case — the same
"served at the cutoff" wording above. `SubscribeOptions.Ctx` makes every wait
`Attach` makes cancellable. S2 adds `snapshot`/`reset`/`event`/`synchronized`
notifications over this and the connection-local barrier `Sync` doesn't give
yet (below, "Responses and events").

**Responses and events.** A command's response is ordered **after** the
events its execution emitted on the same subscription, which follows from the
engine applying a command's effect inside the command call (`03`). Fixtures
and clients may rely on it — **at the wire**. In process this is not a
consequence of the call simply returning: a command's effects are published
through the log's outbox, so the call returning does not mean its events have
been delivered (`engine.Control`'s own doc). `engine.Control.Sync` is how an
in-process caller gets that ordering back — it returns once everything
enqueued before the call is committed (numbered, in the ring, offered to every
subscription, and handed to the journal if one is attached — whose own append
is asynchronous and bounded, and records a gap when saturated). `Sync` alone
does not put those events on a socket's wire ahead of the reply, and neither
does a single serialized writer by itself: the goroutine forwarding the
subscription can still be unscheduled when the handler queues its reply.
**S2's server needs a connection-local barrier**, which `Control` does not
offer yet: `Sync(ctx) error` returns no sequence number. S2 adds one of two
things — a `Sync` that returns the sequence it committed through, so the reply
is queued to the connection's one outbound writer only after every record up
to that number has been queued to that same writer; or a barrier through the
forwarding goroutine itself, acknowledged once it has drained every
subscription record present when `Sync` returned. That is what makes "ordered after its
events" true for a socket client without it ever calling `Sync` itself
(session control S1b, plan §4).

Replay and live use the same per-event representation; nothing is coalesced
in the log (SD-18). Unlike gx, **ask transitions are sequenced events in the
same log**, so they resume exactly; gx lists "an API-owned journal" as future
work and shed papers over its absence with a re-fetch and tombstones on every
reconnect. The roster is ordered separately, by its own epoch and cursor.

## The verb × activity gate

Published in the spec as a table, enforced by the engine (`03`):

| activity | `queue` | `interject` | `send_now` | `cancel` | `session.set` | `cancel_subagent` |
|---|---|---|---|---|---|---|
| starting / replaying | refuse | refuse | refuse | refuse | refuse | allow |
| working | allow | allow if the provider can | allow | allow | allow | allow |
| blocked on an ask | allow | `not_accepting` | `not_accepting` | allow | allow | allow |
| foreign turn (the agent's own) | allow | `not_accepting` | `foreign_turn` | allow, written at once | allow | allow |
| idle | allow | `not_accepting` | allow | `not_accepting` | allow | allow |
| idle **with an ask pending** | allow | `not_accepting` | allow | **allow** | allow | allow |
| failed / cancelling / closing | to be specified in S2 | | | | refuse (after `session.stop`, or while closing) | `not_accepting` once closed |

Asks arrive between turns and during foreign turns too, so cancel is accepted
whenever any ask is pending, whatever the activity. The table is a sketch; S2
derives it from the code's actual states rather than from these three words.

`cancel_subagent` (plan 026 PR 2, `Control.CancelSubagent`) stops one running
sub-agent and leaves its turn going. The engine's only gate on it is a closed
engine; in every other activity the session decides, because it is the one
authority on which of its children are running, and it answers
`unknown_subagent` for an id it holds no running child for. Only a session
whose capabilities carry `SubagentCancel` (native) has the verb; any other
answers `unsupported`.

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

Two cases are pinned because they are otherwise ambiguous: an answer to an ask
whose terminal record has been evicted from the bounded registry is
`already_resolved` (never `unknown_ask`, which means the id was never issued
by this incarnation); and a resend with the same `commandId` but a different
payload is `bad_request`, never a re-execution.

Session control S1b (`internal/engine`) adds six codes and the retry policy
below; plan 025 adds a seventh, `stale_model`, on the settings worker it built
on S1b's; plan 026 PR 2 an eighth, `unknown_subagent`, on the per-sub-agent
stop. All of it is `internal/engine/control.go`'s `Command` doc as built —
copied here, not invented:

| code | one line |
|---|---|
| `unknown_row` | a queued row id the queue no longer holds — sent already, removed by another client, or never existed |
| `unknown_subagent` (plan 026 PR 2) | a stop (`Control.CancelSubagent`) of a sub-agent the session holds no running child for — never issued, or already finished; after a client's own stop it is the race with the child's own end, which a client shows nothing for (at most "already finished"), never an error row. Stored, like `unknown_row` |
| `unknown_command` | a command id at or below its client's evicted high-water mark: recognisably expired |
| `in_progress` | a resend that found its id's command still running: at once for a synchronous command, or, for a blocking one (`Cancel`, `Stop`, `Set`, `Interject`), when a waiting duplicate gives up because its own context ended |
| `stale_model` (plan 025) | a `Set` bound to a model (`Setting.ForModel`) the session has since left, refused before the provider was asked — about this command's own arguments, unlike the gate refusals, which is why it has a code of its own: "not applied, the model changed" |
| `aborted` | a stored answer whose outcome the engine cannot itself vouch for — a command whose call panicked, or a `Set` whose caller's context ended after the settings worker had already claimed it |
| `failed` | every other error the engine does not classify by its own sentinel — a provider/RPC refusal of a `Set`, an interject the agent refused, a cancel that failed — reached this way because the command DID run |
| `index_write` | a command that did what it was asked and could not record it — today only `session.set`'s title write: the rename happened, and only `~/.craze/sessions.jsonl` was not updated |

**The retry policy, by code.** A client decides what a resend means from the
code alone, never from message text:

| code | the command | what a client does |
|---|---|---|
| `unavailable`, `not_accepting`, `in_progress` | did NOT run — a gate refusal (the first two), or, for `in_progress` alone, is still running under this exact id | resend the SAME id (mandatory for `in_progress`, safe for the other two); either gets a genuine first attempt, never a cached echo of finding the gate shut |
| `stale_model` | did NOT run — refused before the provider was asked, because the session had left the model the change was bound to | resend the SAME id once the model is what it should be again; it is judged against the model the session is on then, never a replay of this refusal |
| `aborted` | ran, but its outcome cannot be vouched for | re-read state (or, for a `Set`, watch the stream for its own delta) before deciding anything else; send a NEW id if the command is still wanted — never blindly resend the same work under a new id, or a queued interjection can go in twice |
| `failed` | ran, and failed a plain, definite failure | send a NEW id for another attempt |
| anything else | the command's own stored, stable answer — a success, or a refusal about this request or the resource it named | replayed exactly as the first call got it; resending changes nothing |

The invariant underneath the first row: `unavailable`, `not_accepting`,
`in_progress` and `stale_model` are the only four codes the receipts table
never stores — nothing ran, and the id stays exactly as unseen as before the
attempt (`stale_model` because the condition it names, the session being on
the wrong model, can stop being true; plan 025 X8). Every other code is a
stored answer. One table (`classify` in `control.go`) decides both the code
and whether the answer is stored, so the two can never again disagree about
the same error.

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
