# Protocol Reference

Protocol 1 is craze's control socket: a per-session Unix socket that speaks
JSON-RPC 2.0, one JSON object per line. It is what `craze bridge` exposes to
SSH clients (shed's craze lane, coming in `craze bridge`) and what `craze
attach` speaks to run a second full TUI on a session someone else started.

The socket is local, same-uid only: the server checks the connecting
process's uid before it reads a byte, and refuses anyone else's. There is no
authentication in protocol 1 — `hello`'s `auth` field is reserved for scopes,
which arrive at S6 — and no encryption, because nothing crosses the machine
boundary yet.

Today the endpoint is always a **host**: one process serving one session.
A future **hub** endpoint (S4) will multiplex several hosts behind one
socket and splice a client through to the one it asks for
([The hub splice](#the-hub-splice)); protocol 1 already reserves the shapes
that needs, so no wire change is required to add it.

This page documents protocol 1 as `internal/protocol`, `internal/control`,
`internal/remote` and `internal/fakehost` build it. Where this page and a
craze client disagree, the code is right — file an issue.

## Framing

Every message is a UTF-8 JSON object on its own line, terminated by `\n` (a
trailing `\r` before it is tolerated on input). There is no length prefix and
no batching: a JSON array at the top level is refused as an invalid request.

| direction | limit | over the limit |
|---|---|---|
| client → host (inbound) | 4 MiB | discarded up to the next newline; answered `-32600` with `id: null`. The connection stays open |
| host → client (outbound) | 16 MiB | never sent: a reply that would exceed it is replaced by `failed`, reason `response_too_large` (never truncated silently) |

A client must be able to read a 16 MiB line; `hello`'s result carries both
limits (`limits.inboundLine`, `limits.outboundLine`) so a client never has to
hard-code them. Everything a host writes is bounded under the outbound limit
by construction:

- an event's body is at most `MaxRecordBytes` (8 MiB) plus its envelope;
- a snapshot is at most the request's `snapshotBytes`, itself capped at 8
  MiB;
- `asks.list` returns summaries only (`id`, `kind`, `label`, `openedAt`); a
  full ask (`asks.get`) caps each body string at 256 KiB and sets `truncated`
  when it cut one;
- `session.state`'s queue rows are capped at 32 KiB each, and its settings
  carry only the provider's own (bounded) config catalog.

A host tolerates a final line at end-of-file with no trailing `\n` (a lone
trailing `\r` is stripped): a peer that half-closes right after an
unterminated object still gets its answer. Every line a host itself writes
ends in `\n`.

### The envelope

A request:

```json
{"jsonrpc":"2.0","id":"2","method":"session.attach","params":{"sessionId":"session-fake-1"}}
```

- `id` is the client's own (a number or a string), echoed back verbatim, and
  unique among that connection's requests in flight. It is **not** a command
  id (below) — `id` addresses one JSON-RPC round trip; `commandId` addresses
  an idempotent action that can outlive several of them.
- `params` is always an object; a method that takes nothing may send `{}` or
  omit it.

A reply carries exactly one of `result` and `error`:

```json
{"jsonrpc":"2.0","id":"2","result":{"...": "..."}}
{"jsonrpc":"2.0","id":"2","error":{"code":-32000,"message":"...","data":{"code":"not_accepting","reason":"not_accepting"}}}
```

`id` is `null` only when the request's own id could not be read (a line that
was not JSON, or one too long to parse).

A notification has no `id`:

```json
{"jsonrpc":"2.0","method":"synchronized","params":{"subscription":"s-1","seq":1}}
```

Clients send no notifications in protocol 1.

### Tolerant inbound, strict outbound

- A host **refuses** a params field it does not know: `-32602`, reason
  `unknown_field`, naming the field. A newer client sends an optional
  param only after `hello` has advertised the capability it depends on.
- `hello` is the **one** tolerant method (see [`hello`](#hello)): a host
  ignores any field of `hello`'s params it does not know, at any depth,
  because `hello` is where two versions meet.
- A client **ignores** result and notification fields, event kinds and
  notification methods it does not know. A host's results and notifications
  are otherwise exactly what protocol 1 says — nothing extra rides along
  "just in case".

Rejected: silently ignoring an unknown params field on every method. A
swallowed option can make a client believe it took effect (shed's own rule:
"a swallowed allow becoming a no-op is worse than an error").

## `hello`

`hello` opens the connection. A method protocol 1 does not know is refused
`-32601`, reason `unknown_method`, whether or not `hello` has run yet — that
check comes first. Any method protocol 1 *does* know, sent before `hello`, is
refused `bad_request`, reason `hello_required`.

```json
{"jsonrpc":"2.0","id":"1","method":"hello","params":{"protocols":[1],"client":{"kind":"test","name":"fakehost-wire"}}}
```

| params | |
|---|---|
| `protocols` | every protocol version the client speaks, e.g. `[1]` |
| `client.kind` | required, e.g. `"tui"`, `"shed"` |
| `client.name`, `client.version` | optional, free-form; the server logs nothing a client sends — only the client id it assigns and the resume outcome |
| `client.capabilities` | the client's own capability set; empty in protocol 1 |
| `resume` | `{clientId, token}` to take back a client id this host minted (see [Client ids and resume](#client-ids-and-resume)) |
| `auth` | reserved for S6's scopes; absent or `null` |
| `via` | reserved for a hub or relay that originates the connection itself (S4/S7); absent or `null` |

The host answers with the highest protocol version it shares with the
client. If none is shared, `hello` is refused `bad_request`, reason
`protocol_version`, with `data.result.supported` naming what the host does
speak:

```json
{"jsonrpc":"2.0","id":"1","error":{"code":-32000,"message":"...","data":{"code":"bad_request","reason":"protocol_version","result":{"supported":[1]}}}}
```

### A host's result

```json
{"jsonrpc":"2.0","id":"1","result":{
  "protocol":1,
  "endpoint":{"kind":"host","hostId":"0123456789ab","crazeVersion":"0.0.0-fakehost","pid":4242},
  "clientId":"c-1",
  "token":"52fdfc072182654f163f5f0f9a621d72",
  "resumed":false,
  "capabilities":{"rosterSubscribe":false,"sessionCreate":false,"multiplex":false,"connect":false,"snapshot":true,"attachWhenNow":true},
  "codecs":{"event":1,"snapshot":1},
  "limits":{"inboundLine":4194304,"outboundLine":16777216},
  "retryHorizon":{"commands":1024,"ageMs":600000}
}}
```

`endpoint.kind` tells the two results apart: a **host**'s result always
carries `clientId`, `token`, `resumed` (`false` included — it is never
omitted) and `retryHorizon`. A **hub**'s result carries none of those four —
a hub mints no client ids and keeps no receipts of its own — only `protocol`,
`endpoint`, `capabilities`, `codecs` and `limits`. Client ids, tokens and
receipts are always the **host**'s, even through a hub's splice, so a client
reads those four fields only from a host's `hello`.

`capabilities` here is the **connection**'s capability set (`hello`'s own),
distinct from the **session**'s (the attach reply's — see
[Capabilities](#capabilities)): whether the endpoint can multiplex several
sessions, subscribe to the roster, create a session, or splice a client
through (all `false` on a host in protocol 1), plus whether it serves
`session.snapshot` and an attach with `when: "now"` (both `true`).

### Client ids and resume

`hello` mints a **client id** (`c-1`, `c-2`, …) for this connection and hands
back a **resume token**: 128 random bits, hex, known only to this host and
never logged. A later `hello{resume: {clientId, token}}` on a new connection
to the **same host** takes the id back:

| the resume | answer |
|---|---|
| the token matches, names this client, same engine incarnation | `resumed: true`, the same `clientId` |
| the token names a **different** client | `bad_request`, reason `bad_token` |
| the token is from a previous engine incarnation, or unknown to this host (another host's, retired, expired) | `resumed: false`, a **fresh** `clientId` |

The token is **stable across resumes** — it is never rotated. Rotating it
would make "of two simultaneous resumes the second one wins" (below)
impossible, since the second presents the token the first replaced, and a
client that crashed between the reply and saving a new token would lose its
identity for good. A token is valid only on the host that issued it
(`hello`'s `endpoint.hostId` is what a client checks a resume against, not
just the first host it ever dialed).

A client id is retired — and can never be resumed again — once it has been
released (its connection is gone) for at least the retry horizon (below) and
the host holds no receipt of it, open or running. A binding with **no
connection** for twice that long is dropped even if the client id itself
stays retirable a while longer, so a host does not grow one entry per client
that never comes back. **A resend is never safe unless the host's `hello`
answered `resumed: true`** for this exact client id, on this exact host: any
other answer means the outcome of a command sent before the disconnect is
unknown, and a client must re-read state (`session.state`) rather than
resend it under a fresh id (which risks running it twice).

`retryHorizon` is the size and age bound of the host's command-id table,
which holds one stored answer per command id that actually reached the
engine — every code except `unavailable`, `not_accepting`, `in_progress` and
`stale_model` (`protocol.Retry`), which the engine never stores because
nothing ran under that id. A resend of one of those four is evaluated fresh
every time, not answered from the table. A resend of a command whose answer
**is** stored, within the horizon, gets that stored answer back; past it,
`unknown_command`.

## Sessions and the roster

`sessions.list` returns every session this endpoint serves (one, on a host in
protocol 1):

| result | |
|---|---|
| `epoch` | on a host, the host's own id — a new host process is a new epoch, so a client that sees the epoch change reseeds from scratch |
| `cursor` | the log's committed seq when the rows were read |
| `sessions[]` | one row per session (below) |

A row is the [session info document](#the-session-info-document) plus:

| field | |
|---|---|
| `title` | |
| `activity` | `starting`, `replaying`, `idle`, `working`, `error` or `closing` |
| `foreignTurn` | the agent is running a turn of its own — grok's interjection fallback, or a native sub-agent's wake — during which `activity` reads `idle` |
| `pendingAsks` | how many asks are open |
| `headAsk` | `{id, kind, label}`, the first open ask (absent when none is) |

`activity`/`foreignTurn` answer "is it running"; `pendingAsks`/`headAsk`
answer "is it blocked on me" — two independent signals, because an agent can
be idle *and* waiting on a permission at the same time.

`sessions.subscribe` and `session.connect` are `unsupported` on a host
(reasons `roster_unsupported` and `hub_only`): both belong to the hub.

## Methods

Every session-scoped method carries `sessionId`, the durable craze session
id, at `params.sessionId` (`hello` and `sessions.*` do not). Every mutating
one also carries `commandId`: a canonical positive decimal string, counted
from 1 **per client**, never reused. Resending a mutating method under the
same `commandId` is how a client asks "did that happen?" — see
[Errors and retry](#errors-and-retry).

| method | mutating | params (beyond `sessionId`/`commandId`) | result | notes |
|---|---|---|---|---|
| `hello` | | see [`hello`](#hello) | see [`hello`](#hello) | tolerant; before any other method |
| `sessions.list` | | — | `epoch`, `cursor`, `sessions[]` | |
| `sessions.subscribe` | | — | — | `unsupported` on a host, reason `roster_unsupported`; the hub's (S4) |
| `session.connect` | | `sessionId` | — | `unsupported` on a host, reason `hub_only`; the hub's splice (below) |
| `session.attach` | | `cursor?`, `when?`, `budget?` | `subscription`, `session`, `ready`, `after`, `snapshot?`, `reset?` | see [Attach, resume and snapshots](#attach-resume-and-snapshots) |
| `session.detach` | | `subscription` | `{}` | ends this connection's attachment; the reply is its terminal acknowledgement |
| `session.state` | | — | activity, `foreignTurn`, `turn`, `waiting`, `sendNow?`, `queue[]`, `pendingAsks`, `headAsk?`, `err`, `startFailed`, `prompted`, `cancelled`, `settings` | a read, not a cut of the stream |
| `session.snapshot` | | `agentId?`, `budget?` | `snapshot` | one bounded snapshot, main or one child's, no subscription |
| `session.sync` | | — | `seq` | the reply barrier with no command ([below](#the-reply-barrier)) |
| `session.prompt` | ✓ | `text?`, `fromRow?`, `mode` (`queue`\|`send_now`\|`interject`) | `turn`+`text` \| `queued` \| `armed` \| `{}` | `mode: interject` with `fromRow` is refused `-32602`, reason `bad_request` — interject takes text alone |
| `session.cancel` | ✓ | `turnId?` | `outcome`, `turn`, `reported` | with no `turnId`, cancels **the current turn** — craze's own or the agent's, whichever holds the session when the host validates the call (see [The foreign turn](#the-foreign-turn)); a named turn that is not current is `stale_turn` |
| `session.disarm` | ✓ | — | `{}` | takes back an armed send-now, leaving its text queued |
| `session.queue.add` | ✓ | `text` | `row` | |
| `session.queue.edit` | ✓ | `rowId`, `text`, `expectedVersion?` | `{}` | `expectedVersion` is optional: given and stale, the edit is refused `stale_version` — the check-and-edit that stops two editors from silently overwriting each other; **omitted, the edit is unconditional** and can overwrite a concurrent edit |
| `session.queue.remove` | ✓ | `rowId` | `row` | |
| `session.queue.clear` | ✓ | — | `removed[]` | every row removed, in queue order |
| `session.set` | ✓ | `setting{kind, id?, value, forModel?}` | `value`, `rev` | `rev` is the seq of the state delta that carried the change, `0` if it could not be learned |
| `session.setTitle` | ✓ | `title` | `{}` | the delta, not the reply, is how a client learns the title took |
| `session.subagent.cancel` | ✓ | `agentId` | `{}` | stops one running sub-agent; the rest of the turn goes on; its outcome is the child's `finished` row, as an event |
| `session.stop` | ✓ | — | — | `unsupported` on every host in protocol 1 (capability `stop: false`); S4's headless hosts implement it |
| `asks.list` | | — | `asks[]` (summaries: `id`, `kind`, `label`, `openedAt`) | |
| `asks.get` | | `askId` | `ask` (the full record, body strings capped at 256 KiB) | |
| `asks.answer` | ✓ | `askId`, `answer` | `{}` | the first valid answer wins; an invalid one is `bad_request`, reason `bad_answer`, and leaves the ask open |
| `session.create` | | — | — | reserved for the hub (S4); a host answers `unsupported`, reason `hub_only` |

Deliberately not on the wire: `GiveUp`/`GiveUpDrain` (`craze prompt`'s own
foreign-turn policy, no socket client's concern), `Start`/`Close` (a host's
own lifecycle), a bare `Subscribe` (attach subsumes it) and `NewClientID`
(`hello` does that).

### The reply barrier

Every mutating method's reply, and `session.sync`'s own (it is the barrier
with no command), waits for the **event barrier** before it is queued: the
log's committed head at the moment of the call, forwarded to this
connection's attachment — or the attachment's terminal acknowledgement, if it
ends first — so a client is never handed a reply before the events the
command caused. `seq` (`session.sync`'s result, and every state read's) is
that committed head.

What happens to a reply already admitted, but not yet past this barrier,
differs by how the engine's turn at being "the session" ends:

- An **ordinary end** (the session closes on its own — `session.stop`, the
  provider exiting, an error) drains normally: every admitted reply is still
  written, in order, once its barrier clears, and the connection closes only
  once nothing is owed.
- An **engine replacement** (the host is handed a new session to serve,
  S4/`session/load`) is not drained. The connection becomes **terminal-only**
  from that instant: every ordinary line still queued — a handler's reply, an
  event, a plain `reset` — is dropped, and only the one terminal line
  survives (an attached connection's `reset{session_replaced}`, or a claimed
  detach's `{}`). A handler still running when this happens replies to
  nobody; its client learns the outcome only by resending after a fresh
  `hello` (`resumed: false`).

### `session.snapshot`

Returns one bounded snapshot with no subscription — the same codec and
windowing an attach's own snapshot uses (newest entries first, up to the
budget, with an omitted-entry ledger telling a client what it is missing).
`agentId` names a running sub-agent's own transcript instead of the main
one; an unknown id is `unknown_subagent`. `budget.snapshotBytes` defaults to
4 MiB and is capped at 8 MiB regardless of what is asked.

This is deliberately **not** a page API over older history. An entry's id
names the event that created it, not its latest contents — a tool entry is
updated in place long after it is first written — so paging entries that can
still change needs rules protocol 1 does not yet have. If a client needs
history older than the snapshot window, that arrives later as a new method
behind a new capability; it is not a breaking change.

## Notifications

A notification has no `id` and carries its subscription id in `params`.

| notification | params | meaning |
|---|---|---|
| `event` | `subscription`, `seq`, `event` | one committed event; `event` is the record's body **verbatim** — the same JSON the lossless event codec wrote, never re-encoded |
| `synchronized` | `subscription`, `seq` | the stream has delivered through this attachment's cutoff — shed's "ready" for the *stream* (not to be confused with `ready`, the *session*'s readiness). Sent once per attachment |
| `ready` | `subscription`, `session`, `startFailed`, `err?` | the session's start has finished (`startFailed: false`) or failed (`true`, `err` the text); owed once, only to an attachment made **before** readiness (`when: "now"`, or a `when: "ready"` attach that raced the start) — but not delivered if that subscription resets before its position reaches the seq the start completed at, and not owed at all if the session closes without ever starting (that attachment ends `reset{session_closed}` instead, with no `ready`). `session` is the final [info document](#the-session-info-document) — catalogs and provider session id included |
| `reset` | `subscription`, `reason` | the subscription is over (below) |

A `reset` is a notification, not a gap: nothing is ever silently lost. Every
reason and what a client does about it:

| reason | cause | what a client does |
|---|---|---|
| `slow_consumer` | the client fell behind its own budget | after readiness: re-attach **with** its cursor — the log answers from the ring or the journal, or refuses and a snapshot follows. Before readiness: re-attach `when: "ready"`, with **no** cursor |
| `omitted` | a record no client could ever fold (over the per-record limit) | re-attach with **no** cursor — unless this `omitted` is itself the session's closing reset (an unfoldable record in the final tail): the connection then ends exactly as `session_closed` ends it, and there is nothing to re-attach to |
| `replay_failed` | the journal leg of a cursor replay failed asynchronously | discard everything folded since the cursor; re-attach with no cursor |
| `session_replaced` | the host swapped its engine for a new session | the connection closes ([terminal-only from the swap](#the-reply-barrier): no ordinary reply queued before it survives); reconnect, say `hello` afresh (the old token is void) and attach the new session |
| `session_closed` | the session is over; its final records were delivered first — or, if the journal's tail could not be read, the contiguous prefix of them the host still had | the connection closes; there is nothing to reconnect to |

## Attach, resume, and snapshots

`session.attach` puts a connection on a session's event stream. A connection
holds **at most one** live attachment at a time; a second `session.attach`
while one has not yet fully ended is `bad_request`, reason
`already_attached`.

```json
{"jsonrpc":"2.0","id":"2","method":"session.attach","params":{"sessionId":"session-fake-1"}}
```

| params | |
|---|---|
| `cursor?` | `{incarnation, seq}` — where the client already is. Absent asks for a snapshot |
| `when?` | `"ready"` (default) or `"now"` |
| `budget?` | `{maxItems?, maxBytes?, snapshotBytes?}` — a client may lower the subscription's item/byte budget, or raise it up to the host's own maximum; `snapshotBytes` is capped at 8 MiB |

`when: "ready"` (the default) waits until the session's start has finished —
a `session/load` replay therefore reaches this client as a **snapshot**,
never as a live burst. The wait is bounded by the client's own patience, not
a flat timeout: a load can legitimately take minutes. The host's own bound is
10 minutes, past which the attach is `unavailable`, reason `not_ready` (never
stored — safe to retry). A start that already failed is answered
`not_accepting`, reason `start_failed`, with `data.cause`.

`when: "now"` attaches immediately, the start included: the reply answers
`ready: false` while the session is still starting, the subscription
receives the start as ordinary live events, and the host owes this
attachment one `ready` notification once the gate opens — delivered once the
subscription's position reaches the seq the start completed at. If this
subscription resets before that point (a slow consumer, a replay failure),
the `ready` is dropped, not resent later; and if the session closes without
ever starting, no `ready` is owed at all — this attachment ends
`reset{session_closed}` instead. This is what lets a client watch a session
start.

### The reply

A real instance, from the fixtures
(`internal/fakehost/testdata/wire/01-hello-attach-snapshot.ndjson`), a fresh
attach with no cursor — `session` is the [info
document](#the-session-info-document) below, and `snapshot` is
`transcript.EncodeSnapshot`'s JSON, embedded raw:

```json
{"jsonrpc":"2.0","id":"2","result":{
  "subscription":"s-1",
  "session":{
    "sessionId":"session-fake-1",
    "providerSessionId":"stub-session-1",
    "incarnation":"INCARNATION-1",
    "hostId":"0123456789ab",
    "workspace":"/work",
    "provider":{"name":"","label":"cursor"},
    "catalogs":{
      "models":[{"id":"grok","name":"Grok"},{"id":"fast","name":"Fast"}],
      "modes":[
        {"id":"agent","name":"Agent","description":"Full agent capabilities with tool access"},
        {"id":"plan","name":"Plan","description":"Read-only mode for planning and designing before implementation"},
        {"id":"ask","name":"Ask","description":"Q&A mode - no edits or command execution"}
      ]
    },
    "capabilities":{"interject":false,"subagentCancel":false,"subagentBackground":false,"modes":true,"effort":true,"fastToggle":true,"subagentRows":true,"subagentTranscript":false,"todos":true,"askCards":true,"planCards":true,"parameterizedPicker":true,"cancel":true,"approvals":true,"historyCursor":true,"stop":false},
    "retryHorizon":{"commands":1024,"ageMs":600000}
  },
  "ready":true,
  "after":{"incarnation":"INCARNATION-1","seq":1},
  "snapshot":{
    "version":1,
    "incarnation":"INCARNATION-1",
    "seq":1,
    "settings":{
      "mode":"agent",
      "model":"grok",
      "config":{
        "options":[
          {"id":"effort","name":"Effort","category":"thought_level","type":"select","current":"medium","selectValues":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"},{"value":"high","name":"High"}]},
          {"id":"fast","name":"Fast","category":"model_config","type":"select","current":"false","selectValues":[{"value":"false","name":"Off"},{"value":"true","name":"Fast"}]}
        ]
      },
      "commands":{"commands":[{"name":"research","description":"Agent-advertised command"}]}
    },
    "main":{}
  }
}}
```

- `subscription` is a server-assigned id (`s-1`, `s-2`, …), unique per
  connection, carried on every notification of this attachment.
- `after` is where the stream continues: the client's own cursor when it was
  honoured, otherwise the snapshot's `{incarnation, seq}`. Everything at or
  below `after` is already covered by the cursor's replay or by `snapshot`.
- `snapshot` is present exactly when the cursor was **not** honoured (absent,
  refused, or none was given).
- `reset` (a `CursorReason`, see below) is present when a cursor **was**
  given and refused — the reason the first refusal named.
- The reply is on the wire, and `after` decided, before the very first
  `event` of this attachment: a client never sees an event whose position it
  cannot place.

### Cursors and resets

A cursor is `{incarnation, seq}`: a log incarnation and the seq of the last
event this client folded. A cursor from **another** incarnation is refused
synchronously (`foreign_incarnation`) and answered with a fresh snapshot. The
full set of refusal reasons:

`foreign_incarnation`, `future_seq`, `backlog_too_large`, `no_journal`,
`journal_gap`, `journal_behind`, `evicted`.

Any of these on the attach reply itself means: take the snapshot that came
with it, forget the old cursor, and go on from `after`. The same reasons,
arriving later as a `reset{reason: "replay_failed"}` or `{reason:
"omitted"}` notification, mean: re-attach with no cursor.

### Re-attaching, and the per-episode bound

A client that loses its connection and gets it back re-attaches with its
**held** cursor (the last seq it has actually received and queued, in
order — never a seq it has merely heard about). Resending an in-flight
command after a reconnect is safe **only** when the reconnecting `hello`
answered `resumed: true` for the same client id, token and host; any other
answer means the command's outcome is unknown, and no client should ever
resend a command whose earlier attempt it cannot rule out having run.

A client bounds how many times it will re-attach within one "episode" (a
connection loss and its resulting resets, resends and retries) — 8
re-attaches, counted from every reset, reconnect and retried refusal, reset
to zero the moment the stream reaches `synchronized` again
(`ReattachesPerEpisode`).

That is the **stream's** bound. Getting a connection back at all is bounded
separately, in attempts and time: a reconnect episode makes at most 3 dial
attempts, and ends 10 s after the loss, whichever comes first
(`remote.Options.Redials`/`RedialWindow`) — every dial, handshake and resend
of an in-flight command bounded by what is left of it. Past either bound,
a client gives up on this attempt and surfaces the outcome as unknown
(`resume_lost` or `disconnected` — see [Errors](#errors-and-retry)) rather than
looping forever against a host that keeps resetting it, or that it cannot
reach at all.

### Bounded history is `session.snapshot`, not pages

There is no `history(beforeSeq, limit)` method in protocol 1 — see
[`session.snapshot`](#sessionsnapshot) above. The attach snapshot's window
(the tail, up to 8 MiB) is everything a fresh client gets; anything older
than that is future work behind a new capability.

## The session info document

Carried by an attach reply's `session`, a `ready` notification's `session`,
and the first half of a `sessions.list` row. It is **final** once the
session is up — its catalogs are empty and `providerSessionId` may be `""`
until then.

```json
{
  "sessionId":"session-fake-1",
  "providerSessionId":"stub-session-1",
  "incarnation":"INCARNATION-1",
  "hostId":"0123456789ab",
  "workspace":"/work",
  "provider":{"name":"","label":"cursor"},
  "catalogs":{"models":[{"id":"grok","name":"Grok"}],"modes":[{"id":"agent","name":"Agent","description":"Full agent capabilities with tool access"}]},
  "capabilities":{"interject":false,"subagentCancel":false,"subagentBackground":false,"modes":true,"effort":true,"fastToggle":true,"subagentRows":true,"subagentTranscript":false,"todos":true,"askCards":true,"planCards":true,"parameterizedPicker":true,"cancel":true,"approvals":true,"historyCursor":true,"stop":false},
  "retryHorizon":{"commands":1024,"ageMs":600000}
}
```

Every field above is real (drawn from the same fixture as [the attach
reply](#the-reply)); `capabilities`' fields are documented in full under
[Capabilities](#capabilities) below.

| field | |
|---|---|
| `sessionId` | the durable craze session id every session-scoped call names |
| `providerSessionId` | the agent's own id; changes on a `session/load` |
| `incarnation` | the event log's id: the scope of seqs, turn ids and ask ids, and a cursor's first half |
| `hostId` | the host serving this session |
| `workspace` | the session's working directory |
| `provider` | `{name, label}` — `name` is the ACP provider id (`cursor`, `grok`, `gx`, or `native`'s own), `label` what a client shows |
| `catalogs` | `{models[{id,name}], modes[{id,name,description}]}` — empty until the session is ready |
| `capabilities` | the session's own capability set, below |
| `retryHorizon` | `{commands, ageMs}` — the command-id table's size and age bound |

## Capabilities

Two independent levels: what the **connection** (the endpoint) can do, and
what **this session** can do.

**Connection** (`hello`'s `capabilities`): `rosterSubscribe`, `sessionCreate`,
`multiplex`, `connect` — all `false` on a host in protocol 1, the hub's
alone — plus `snapshot` and `attachWhenNow`, both `true`.

**Session** (the info document's `capabilities`): every field of the
engine's own capability set, in its wire name, plus four the protocol states
for every host of protocol 1:

| wire name | meaning |
|---|---|
| `interject` | the provider can fold text into a running turn without cancelling it |
| `subagentCancel` | `session.subagent.cancel` is served |
| `subagentBackground` | the provider can run a sub-agent's turn in the background (a native "wake") |
| `modes` | the provider has switchable modes |
| `effort` | the provider has an effort/thinking-level setting |
| `fastToggle` | the provider has a fast-mode toggle |
| `subagentRows` | sub-agent rows are reported at all |
| `subagentTranscript` | a sub-agent's own transcript can be read (`session.snapshot{agentId}`) |
| `todos` | the provider reports a todo list |
| `askCards` | the provider can ask permission/question cards |
| `planCards` | the provider can ask plan-approval cards |
| `parameterizedPicker` | the config catalog can offer per-model options |
| `cancel` | `true` on every host in protocol 1 |
| `approvals` | `true` on every host in protocol 1 |
| `historyCursor` | `true` on every host in protocol 1 |
| `stop` | `false` on every TUI-hosted session; S4's headless hosts set it `true` |

A client hides — never merely disables — whatever a capability says this
session cannot do. A capability the engine's own `agent.Capabilities` grows
later always gets a wire name here before it is used: every field of that Go
struct has a matching entry in this table, mechanically checked.

## Versioning

The protocol is **one integer**, `1`, deliberately decoupled from craze's own
release version — a client and a host can be built from very different
commits and still speak the same protocol. A `hello` whose `protocols` share
none with the host's is refused (see [`hello`](#hello)).

The event and snapshot codecs carry their own version numbers in `hello`'s
`codecs` (and the session info document does not repeat them): a client that
does not recognize a codec version must not attempt to fold that stream — it
can still forward it opaquely, but not interpret it.

New behaviour is always announced as a **capability**, never inferred from a
version number: a client checks `capabilities.foo`, never "am I talking to a
build recent enough to have foo".

## The foreign turn

`foreignTurn` (on `sessions.list` rows, `session.state`, and inside a
snapshot) says the agent is running a turn of its own that craze did not
start: grok's interjection fallback, or a native sub-agent's background
"wake". While one runs, the engine's own `activity` reads `idle` — a foreign
turn is not itself an activity, it overlays whichever one is current.

On the event stream, a foreign turn is bracketed by two
`foreign_turn` events, `{id, text, reason?, running}`:

```json
{"type":"foreign_turn","id":"wake-1","text":"sub-agent result","reason":"subagent_wake","running":true, "at":"..."}
{"type":"foreign_turn","id":"wake-1","reason":"subagent_wake","running":false, "at":"..."}
```

`reason` is an **open string** — not a closed enum — with two values known
today: `""` (grok's fallback) and `subagent_wake` (a native child's wake). An
unknown value renders with the default wording; a client must not fail on
one it has never seen.

A foreign turn has **no `EventDone` and no `EventError`**: nothing about how
it ended is on the wire beyond `running: false`. A client learns only that it
is over, never its outcome. `foreignTurn{running: false}` always precedes the
next turn's own output on the stream — an agent's own next turn (a queued
follow-up draining, say) never races ahead of the foreign turn's ending.

A `foreign_turn` ending with `next: ""` and a non-empty queue does **not**
mean the queue is stuck: it can equally mean a queued row was restored to
the head of the queue and will drain on its own paced retry. A client tells
the two apart by `session.state.activity` (idle means it will drain; a
genuine stall shows an error), or by seeing a `queued` event for that same
row, at position 0, carrying the row's own cause, immediately before the
ending (the same batch, adjacent seqs).

**`session.cancel` with no `turnId` cancels whatever turn currently holds the
session** — craze's own, or the agent's foreign one — whichever the host is
looking at the instant it validates the call. There is a small residual
window here (tracked as follow-up **SF-48**): if a foreign turn ends and its
owed drain claims a queued row between the moment a client decided to cancel
and the moment the host validates that cancel, the cancel lands on the row's
new turn (which is cancelled, not restored) rather than on nothing. The
eventual fix is additive — a `session.cancel{expect: "foreign"}` that answers
`stale_turn` if the turn has already moved on — behind a future capability,
not in protocol 1.

## Errors and retry

A refused call's `error` object:

```json
{"code":-32000,"message":"engine: not accepting that now","data":{"code":"not_accepting","reason":"not_accepting"}}
```

| field | |
|---|---|
| `code` | the JSON-RPC integer: `-32700` parse error, `-32600` invalid request (a batch, an over-long line, a missing method), `-32601` unknown method, `-32602` invalid params, `-32000` for every craze-level refusal |
| `message` | for people; never matched on |
| `data.code` | the **closed set** a client decides from — see [Codes and retry](#codes-and-retry) |
| `data.reason` | a second, finer closed set naming the exact sentinel — for wording and for reconstructing the underlying Go error, **never** for retry |
| `data.result` | a result carried beside the error: `session.cancel`'s `{outcome, turn, reported}`, or `hello`'s `{supported}` on `protocol_version` |
| `data.cause` | a wrapped failure's own text — `session.setTitle`'s index-write half, or a start failure |

**A client decides everything from `data.code` alone.** `data.reason` is
richer but is not part of the retry contract; a client that only knows `code`
still behaves correctly.

### Codes and retry

| code | the command | resend the same `commandId`? |
|---|---|---|
| `unavailable` | did not run — a gate refusal | yes |
| `not_accepting` | did not run — a gate refusal | yes |
| `in_progress` | still running under this exact id | yes (mandatory: it is the safe way to learn the answer once it lands) |
| `stale_model` | did not run — refused before the provider was asked, because the session had already left the model the change was bound to | yes, once the session is back on that model — it is judged fresh, never as a replay of the refusal |
| every other code | **ran** (or is this command's own stored answer) — *when* the id ever reached the engine | no — a resend replays the stored answer; send a **new** `commandId` for another attempt |

That "ran" column is only true once a `commandId` has reached the engine.
`bad_request`, `unknown_session` and `unsupported` can also be returned
before that — a malformed request, an unknown session, or an unsupported
method are refused by the socket layer itself (`internal/control/dispatch.go`,
`handlers.go`), with no command receipt at all. Resending one of those is
just resending the same malformed or unsupported request, never a replay of
a stored answer.

`aborted` is the one exception worth calling out: the command ran, but its
outcome cannot be vouched for (a caller's context ended mid-flight, a call
that panicked). Re-read state before deciding anything else; never blindly
resend the same *intent* under a new id, or a queued interjection can go in
twice.

Retrying is also bounded by the **connection**: after a reconnect, a client
resends an in-flight command only when the reconnecting `hello` answered
`resumed: true` for the exact client id, token and host it held before —
see [Attach, resume, and snapshots](#attach-resume-and-snapshots).

### The reason table

Every reason a host sends, grouped by its one code. Client-side reasons (last
group) are named here so a client never mistakes one for a host's own answer
— no host ever sends them.

| code | reasons a host sends |
|---|---|
| `not_accepting` | `not_accepting`, `not_in_turn`, `start_failed` |
| `aborted` | `command_aborted`, `set_outcome_unknown`, `bad_catalog`, `context` |
| `unavailable` | `log_backed_up`, `ask_unavailable`, `set_unavailable`, `not_run`, `attach_raced`, `not_ready`, `busy` |
| `failed` | `option_gone`, `failed`, `response_too_large`, `snapshot_too_large` |
| `bad_request` | `bad_request`, `bad_answer`, `hello_required`, `unknown_field`, `line_too_long`, `protocol_version`, `bad_token`, `already_attached` |
| `unsupported` | `unsupported`, `unknown_method`, `stop_unsupported`, `roster_unsupported`, `hub_only` |
| `unknown_session` | `unknown_session` |
| `unknown_ask` | `unknown_ask` |
| `already_submitted` | `already_submitted` |
| `already_resolved` | `already_resolved` |
| `queue_full` | `queue_full` |
| `text_too_long` | `text_too_long` |
| `prompt_in_flight` | `prompt_in_flight` |
| `foreign_turn` | `foreign_turn` |
| `prompt_cancelled` | `prompt_cancelled` |
| `stale_version` | `stale_version` |
| `stale_turn` | `stale_turn` |
| `unknown_row` | `unknown_row` |
| `unknown_command` | `unknown_command` |
| `in_progress` | `in_progress` |
| `stale_model` | `stale_model` |
| `index_write` | `index_write` |
| `unknown_subagent` | `unknown_subagent` |
| *(client-side; no host ever sends these)* | `resume_lost`, `disconnected` (both `ErrOutcomeUnknown` after a reconnect — see above), `no_answer` (a client-local deadline passing with no reply at all) |

### The code → shed `LaneError` table

`shed_core::lane::LaneError` (`shed/crates/shed-core/src/lane.rs`) is shed's
one closed error type across every agent lane it drives, craze's included.
It is deliberately smaller than craze's own code set — nine variants against
craze's twenty-three — so several craze codes map onto the same
`LaneError`, and the mapping below is craze's own judgment call about which,
not something the wire states. A craze client that is not shed can use
craze's own codes directly and does not need this table.

| craze `data.code` | `LaneError` | why (where not obvious) |
|---|---|---|
| `bad_request` | `BadRequest` | |
| `unknown_session` | `UnknownSession` | |
| `unknown_ask` | `UnknownApproval` | craze's "ask" is shed's "approval" |
| `already_submitted` | `AlreadySubmitted` | |
| `already_resolved` | `AlreadyResolved` | |
| `not_accepting` | `NotAccepting` | did not run — a gate refusal; `protocol.Retry` is true, and craze resends the **same** `commandId`, which is exactly `NotAccepting`'s own posture: "not yet, try later" |
| `foreign_turn` | `NotAccepting` | "the session will not take this send-now right now" is exactly `NotAccepting`'s posture |
| `in_progress` | `NotAccepting` | did not run under a new attempt — it is this command's own attempt, still running; `protocol.Retry` is true (mandatory: it is the safe way to learn the answer). A resend of a command still running reads the same way to a caller: "not yet, try later" |
| `stale_model` | `NotAccepting` | did not run — refused before the provider was ever asked, because the session had already left the model the change was bound to. `protocol.Retry` is true: craze resends the **same** `commandId` once the session is back on that model, judged fresh, never as a replay. This is not an argument error — `NotAccepting`'s "will not take this right now" is the closer shed posture, and it is the closest fit shed has, not an exact one: a shed caller should still consult craze's own `data.code` (and `protocol.Retry`) rather than assume shed's `NotAccepting` alone carries the "resend once the model comes back" rule |
| `unavailable` | `Unavailable` | did not run — a gate refusal; `protocol.Retry` is true, and `LaneError::Unavailable`'s own doc ("nothing to talk to… quiet") already reads as retryable rather than terminal |
| `unsupported` | `Failed` | reachable by ignoring a capability already `false`, **or** by sending a method protocol 1 does not know at all (`unknown_method`, no capability involved); `LaneError` has no dedicated variant for either |
| `stale_version` | `BadRequest` | about this command's own arguments (an edit against a row version that moved), the same posture as a malformed request; `protocol.Retry` is false — it ran (was refused) and a resend replays that refusal |
| `stale_turn` | `BadRequest` | a cancel naming a turn that is no longer current — likewise about the command's own arguments; `protocol.Retry` is false |
| `queue_full`, `text_too_long`, `prompt_in_flight`, `prompt_cancelled`, `unknown_row`, `unknown_command`, `unknown_subagent`, `aborted`, `failed`, `index_write` | `Failed` | no `LaneError` variant models a queue, a row, a sub-agent or "ran but the outcome cannot be vouched for"; the craze code's own text, kept in `Failed`'s string, is what a shed client actually shows |
| *(unused)* | `Unauthorized` | protocol 1 has no authentication (`hello.auth` is reserved for S6); no craze code maps to it today |

## The gate table

What each of `internal/engine`'s own gated commands answers in every state
the engine can be in — the table's columns are its mutating methods (every
one but `session.stop`, unsupported on every host in protocol 1). It is not
every method protocol 1 defines: reads (`session.state`, `session.snapshot`,
`sessions.list`, `asks.list`, `asks.get`), the attachment methods
(`session.attach`, `session.detach`), `hello`, and the methods a host never
serves are answered as their own sections above describe, not by this table.
This table is
rendered mechanically from `internal/engine`'s own
`TestTheGateTableIsTheEngines` — the same test that drives every cell through
the real engine — by `gateTableMarkdown()`, and
`TestPublishedGateTableIsTheTested` fails the build if the section below
drifts from that render. Do not hand-edit between the markers.

<!-- gate-table:begin -->
| state | prompt queue | prompt send_now | interject | cancel | disarm | queue verbs | set | setTitle | answer | subagent.cancel |
|---|---|---|---|---|---|---|---|---|---|---|
| starting | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting`; allowed if an ask is pending; allowed if a foreign turn runs; `stale_turn` naming a turn that is not current | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` | allowed | allowed — the session's answer |
| replaying | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting`; allowed if an ask is pending; allowed if a foreign turn runs; `stale_turn` naming a turn that is not current | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` | allowed | allowed — the session's answer |
| idle | starts | starts | `not_accepting` (`not_in_turn`); `unsupported` if the provider cannot interject | `not_accepting`; allowed if an ask is pending; allowed if a foreign turn runs; `stale_turn` naming a turn that is not current | `not_accepting` — nothing is armed | allowed | allowed | allowed | allowed | allowed — the session's answer |
| working | queues | arms; `already_submitted` if one is armed | allowed; `unsupported` if the provider cannot interject | allowed; `stale_turn` naming a turn that is not current | `not_accepting`; allowed if a send-now is armed | allowed | allowed | allowed | allowed | allowed — the session's answer |
| cancelling | queues | arms; `already_submitted` if one is armed | allowed — the session's answer: it has not been told of the cancel yet; `unsupported` if the provider cannot interject | allowed; `stale_turn` naming a turn that is not current | `not_accepting`; allowed if a send-now is armed | allowed | allowed | allowed | allowed | allowed — the session's answer |
| waiting | queues | `foreign_turn` | `not_accepting` (`not_in_turn`) — the session's answer: no turn of craze's is open | allowed; `stale_turn` naming a turn that is not current | `not_accepting` — nothing can be armed | allowed | allowed | allowed | allowed | allowed — the session's answer |
| error (start failed) | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting`; allowed if an ask is pending; allowed if a foreign turn runs; `stale_turn` naming a turn that is not current | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` | allowed | allowed — the session's answer |
| stopped | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting`; allowed if an ask is pending; allowed if a foreign turn runs; `stale_turn` naming a turn that is not current | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` | allowed | allowed — the session's answer |
| closing | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` — always, even with a foreign turn running or a turn named | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` | allowed — the registry is still open: the session's own close, not yet run, is what ends every ask; `unknown_ask` naming an ask never opened | `not_accepting` |
| closed | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` — always, even with a foreign turn running or a turn named | `not_accepting` | `not_accepting` | `not_accepting` | `not_accepting` | `already_resolved` — the close ended every ask | `not_accepting` |
| + foreign turn | queues | `foreign_turn` | `not_accepting` (`not_in_turn`) — the session's answer | allowed — written at once; `stale_turn` naming a turn that is not current | `not_accepting` — nothing can be armed | allowed | allowed | allowed | allowed | allowed — the session's answer |
| + ask pending | as the row | as the row | as the row | allowed — in every row but closing or closed, where e.closed refuses it outright | as the row | as the row | as the row | as the row | as the row | as the row |
<!-- gate-table:end -->

`waiting` is `craze prompt`'s own foreign-turn retry policy (it holds a
refused claim and retries once the foreign turn ends); no socket client sees
it — the TUI shows the plain refusal instead, and a socket client sees
`foreign_turn` from `session.prompt{mode: send_now}` exactly as the `+
foreign turn` row shows.

## The hub splice

Reserved now so protocol 1 needs no change when the hub (S4) arrives. A
connection to a hub starts in **hub mode**: `hello` (a hub's result, no
client id, no receipts), then `sessions.list` and `sessions.subscribe`.

`session.connect{sessionId}` makes the hub dial that session's host and
splice the two connections together: the hub answers `{}`, and from the next
byte on, the client is talking **directly to the host** — starting with its
own `hello`. So client ids, tokens and receipts, always the host's, are
minted by the host the splice lands on, and a resumed connection resumes with
that same host, never with the hub.

A per-session host (every host in protocol 1 today) answers `session.connect`
`unsupported`, reason `hub_only`: a client dialing a host directly is already
where the splice would have put it.

## The published schema

Every method, notification and shared document has a hand-written [JSON
Schema](https://json-schema.org/) (2020-12) file, embedded in
`internal/protocol` and checked by reflection against the Go wire types both
ways (`TestSchemaCoversEveryWireField`) — a field the code sends that the
schema does not describe, or vice versa, fails the build — with one named
exception: `session.create` is reserved for the hub (S4) and has no params,
result, or schema file of its own; a host answers it `unsupported`, reason
`hub_only`, like `session.connect`. The event and snapshot codecs' own
hand-shaped wire structs get the same treatment in `internal/agent`
(`TestEventSchemaCoversTheCodec`) and `internal/transcript`
(`TestSnapshotSchemaCoversTheCodec`), since this package cannot import
either.

The copy published here, under `reference/protocol/schema/`, is byte-for-byte
the same as the one `internal/protocol` embeds
(`TestPublishedSchemaIsTheEmbedded` fails the build otherwise — it walks both
directories recursively, so a file or directory added on either side, at any
depth, fails the build, not only one added directly under `schema/`). Every file's
`$id` is `https://charliek.github.io/craze/reference/protocol/schema/<name>`,
so a `$ref` between files resolves at this URL exactly as it does against the
embedded copy — e.g. [`hello.json`](protocol/schema/hello.json),
[`event.json`](protocol/schema/event.json),
[`session.attach.json`](protocol/schema/session.attach.json).

## Fixtures and the fake host

`internal/fakehost/testdata/wire/*.ndjson` is thirteen scripted scenarios
against a real `internal/control` server over a real engine (wrapping the
TUI's own `Stub`, never a fixture-only re-implementation) — hello and a fresh
attach; a cursor resume and its replay; a foreign-incarnation cursor; a
`slow_consumer` reset and cursor re-attach; an ask answered twice; an invalid
answer; a cancel on idle; a resent `commandId`, replayed and then mismatched;
two clients sharing one prompt; four kinds of refusal; a snapshot of the main
transcript and a child's; an `omitted` reset; and a `hello` resume with a
right token, a wrong one, and one aged past its bound. Every line is
`{"conn": N, "dir": "c2s"|"s2c", "msg": {...}}`, plus `{"dir": "op", "op":
{...}}` lines that are not wire messages at all — they script the host
directly (emitting text, opening an ask, restarting the engine into a fresh
incarnation, stalling or dropping connections). `TestWireFixtures` replays
every one of them byte for byte, validating every line against the schema
above as it sends or reads it — except a c2s line fixture 10 marks
`"invalid": true`: deliberately not a well-formed request of a method
protocol 1 defines with today's params (an unknown method, or a field no
schema allows), sent as it stands and held to no request schema, since it is
designed never to pass one.

`cmd/craze-fake-host` is the same host as a standalone binary, for anyone
scripting against protocol 1 without Go: `craze-fake-host --socket PATH`
prints one ready line on stdout,

```json
{"socket":"/tmp/.../host.sock","sessionId":"session-fake-1","hostId":"0123456789ab"}
```

then reads the same NDJSON ops described above from stdin —
`text`, `thought`, `tool`, `permission`, `question`, `plan`, `end`,
`foreign_turn`, `stall_writes`, `resume_writes`, `drop_connections`,
`restart` (a new incarnation of the same session), `quit`, plus a few the
fixtures alone need (`spawn_subagent`, `oversized_event`, `advance_clock`,
`hang_next`). Its clock, ids, host id, craze version, pid and token source
are all deterministic by default, so a script against it produces the same
wire traffic on every run and every machine.

## What's next

An SSH exec (what `craze bridge` will expose the socket to) needs its own
assumptions about environment and discovery — that lands with `craze bridge`
itself, documented alongside it.
