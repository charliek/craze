# Protocol Reference

Protocol 1 is craze's control socket: a per-session Unix socket that speaks
JSON-RPC 2.0, one JSON object per line. It is what [`craze
bridge`](cli.md#craze-bridge) exposes to SSH clients (shed's craze lane) and
what `craze attach` speaks to run a second full TUI on a session someone else
started.

The socket is local, same-uid only: the server checks the connecting
process's uid before it reads a byte, and refuses anyone else's. There is no
authentication in protocol 1 — `hello`'s `auth` field is reserved for scopes,
which arrive at S6 — and no encryption, because nothing crosses the machine
boundary yet.

An endpoint is one of two kinds. A **host** is one process serving one
session — by default a detached [`craze serve`](cli.md#craze-serve) the
ordinary `craze` spawned, or, with detaching off, the TUI's own process. The
**hub** (S4b) is the machine's one place that knows every host: it lists
them all ([The hub's roster](#the-hubs-roster)) and splices a client through
to the one it asks for ([The hub splice](#the-hub-splice)). It never
multiplexes sessions on one connection, and it never serves a session's
methods itself: those are always the host's. `hello`'s `endpoint.kind` says
which one answered.

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
  carry only the provider's own (bounded) config catalog and, for a native
  session, the fixed-size [usage section](#usage).

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
`session.snapshot` and an attach with `when: "now"` (both `true` on a host).

### The hub's result

```json
{"jsonrpc":"2.0","id":"1","result":{
  "protocol":1,
  "endpoint":{"kind":"hub","hostId":"0a1b2c3d4e5f","crazeVersion":"0.4.0","pid":5150},
  "capabilities":{"rosterSubscribe":true,"sessionCreate":true,"multiplex":false,"connect":true,"snapshot":false,"attachWhenNow":false},
  "codecs":{"event":1,"snapshot":1},
  "limits":{"inboundLine":4194304,"outboundLine":16777216}
}}
```

`endpoint.hostId` is the hub's own id — twelve hex digits, minted per hub
process — and the [roster](#the-hubs-roster)'s `epoch`: a client that sees it
change is talking to a new hub, and reseeds. The hub serves the roster
subscription (`rosterSubscribe`), [session creation](#sessioncreate)
(`sessionCreate`) and the splice (`connect`); `snapshot` and
`attachWhenNow` are `false` because a session's methods are its host's,
reached through the splice; `multiplex` is `false` because one connection
carries the roster or one spliced session, never several. `sessionCreate` is
`true` only on a hub that serves `session.create` — `false` on a hub from
before it existed: a client checks it, never the hub's version.

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

`sessions.list` returns every session this endpoint knows. On a host, that
is its one session (below); on the hub, every host on the machine — [the
hub's roster](#the-hubs-roster), whose rows wrap these.

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
| `headAsk` | `{id, kind, label, summary?}`, the first open ask (absent when none is); `summary` is a [row fact](#the-row-facts) |
| `lastTurn` | how the last turn ended — [below](#the-last-turn); absent while a turn runs, before any has ended, and from an older host |
| `doing`, `lastReply`, `since`, `startFailed`, `startErr`, `prompted` | the [row facts](#the-row-facts), where the session capability `rowFacts` is `true` |
| `attached` | where the session capability `presence` is `true`: how many clients are attached as the row is read — the count a [`presence`](#presence) notification carries; absent is `0` on such a host, and unknown on any other |

`activity`/`foreignTurn` answer "is it running"; `pendingAsks`/`headAsk`
answer "is it blocked on me" — two independent signals, because an agent can
be idle *and* waiting on a permission at the same time.

### The row facts

A host whose session capability `rowFacts` is `true` — every craze from plan
030's session list on, detached or TUI-hosted — puts on its row what a list of
every session needs to say what each one wants without attaching to any. Each
is omitted when unset, so on such a host an absent one is its zero (`""`,
`false`); on a host without `rowFacts` (an older one) none is there and a
client reads the row as it was.

| field | |
|---|---|
| `headAsk.summary` | what the head ask is about: a permission's command or tool title, a question's first question (its title when it asks none), a plan's name. Never on `session.state`'s head ask |
| `doing` | what a working session is doing: the title of the most recently started tool of the running turn that is still running (pending or in progress; its name when it has no title), else `Responding` while the agent's text streams, else `Thinking`. Present only while a turn — craze's own or a [foreign](#the-foreign-turn) one — is working |
| `lastReply` | the first line of the last completed assistant message |
| `since` | when the row entered its current state, on the host's clock, UTC — the first of these that holds: **needs you** (`pendingAsks` > 0), **failed** (`startFailed`, or `lastTurn.outcome` failed — or `activity` error with neither `lastTurn` nor `foreignTurn`: a failure whose ending is still on its way), **working** (`activity` starting, replaying, working or closing, or `foreignTurn`), **idle**. A turn that follows another from the queue in the same settlement keeps the first one's |
| `startFailed`, `startErr` | the session's start failed, and the first line of its error |
| `prompted` | a turn has been started at all |

Every string is one line — the first non-blank one, trimmed, tabs expanded,
control characters dropped — of at most 200 terminal cells, an ellipsis
ending one that was cut. The host computes them from its engine's own
transcript model and ask registry when the row is asked for: like the rest
of the row, a read, not a cut through the stream.

### The last turn

`lastTurn` — on a `sessions.list` row and in `session.state`, never in a
snapshot, whose codec stays at version 1 — is `{outcome, err?, endedAt,
turnId}`: how the session's most recent turn ended, whether it was craze's
own or one the agent ran itself (a [foreign turn](#the-foreign-turn)).

| field | |
|---|---|
| `outcome` | `done` (ran to its end — and every foreign turn, whose closing bracket carries no outcome), `failed` (its ending carried an error), or `cancelled` (a cancel, a prompt withdrawn before it was sent, or the session's close) |
| `err` | a failed turn's error text; absent otherwise |
| `endedAt` | the ending event's time, UTC |
| `turnId` | the ended turn's id as the stream names it: `turn.id` on its `started`/`ended` events for a turn of craze's own, the `foreign_turn` bracket's `id` for the agent's own |

It is kept from the stream's own turn events, in commit order, and **a turn
starting supersedes it**: from a turn's `started` (or a foreign turn's
running bracket) it is absent until that turn ends. So a `lastTurn` that is
present always names the latest turn, with nothing run since — a list shows
"failed" from it without asking whether a turn came after. A client that
reads it after a restore (a reconnect's fresh snapshot carries no ending)
applies it only if it has folded no later turn since, by `turnId`.

`sessions.subscribe` and `session.connect` are `unsupported` on a host
(reasons `roster_unsupported` and `hub_only`): both belong to the hub.

### The hub's roster

The hub learns hosts from the machine's registry (see [The
registry](#the-registry)) and reads each one's own `sessions.list`; it lists
every live host that has a craze session id, keyed by host id, in host-id
order. It polls as the session list does — a round at most every second that
reads the registry (sweeping hosts that died, so a crashed host's row leaves
within one round) and asks every host, eight at a time, 500 ms each, over a
connection it keeps — but only while someone wants the roster: a
subscription, or a `sessions.list` or `sessions.subscribe` waiting for its
answer. Its poll connections say `hello` as client kind `hub`.

A `sessions.list` or `sessions.subscribe` answers once the poll's current
round has heard from every host it lists — an answer, or a failure — and at
most a second after it was asked: a host not heard from by then is listed
`connecting`. A hub that was not polling starts a round for it, so its
answer is what the hosts say now. A hub that cannot read the registry — its
last read failed, and none has succeeded since the answer's round began —
answers both `unavailable`, reason `host_unreachable` (as `session.connect`
does then), never an empty roster. Its `sessions.list` result:

| result | |
|---|---|
| `epoch` | the hub's own id (its `hello`'s `endpoint.hostId`): a new hub process is a new epoch, and a client reseeds |
| `cursor` | the hub's roster sequence, an unsigned 64-bit integer: bumped by every change to a row's content (an `approximate` flip included); it only increases within an epoch, and may jump |
| `sessions[]` | one **roster row** per host (below) |
| `truncated` | `true` when the roster was cut at 512 rows; absent otherwise |

`sessions.list`'s result is a host's or the hub's — the schema says
`anyOf`, since an empty roster is both — and a client knows which from the
endpoint it said `hello` to. A roster row:

| field | |
|---|---|
| `hostId` | the host's id, twelve lowercase hex digits: the row's key, stable from the host's first appearance in the registry |
| `sessionId` | the host's craze session id, a token of at most 128 characters of `[A-Za-z0-9._-]`: what [`session.connect`](#the-hub-splice) and `craze bridge --session` take |
| `host` | what the hub knows about the host beside its row: `pid`, `protocol`, `provider` (its name, at most 64 characters), `workspace` (at most 4096), `startedAt` and `ready` from its registry entry, and `crazeVersion` (at most 128) from its `hello` (`""` until the hub's first) |
| `status` | `connecting` (not yet attempted), `reachable` (the last attempt read its row) or `unreachable` (the last attempt failed) |
| `approximate` | the row is not fresh — its last successful read is more than 3 s old, or the host is not reachable — or the hub cut or dropped something to keep it within its bounds (below) |
| `row` | the host's own `sessions.list` row, **as the JSON value the host sent** — not re-encoded — so a newer host's members this build does not know, at any depth, pass through the hub untouched; absent until the hub's first read, and while the host is unreachable the last one read |

No socket path crosses the wire: a local client that needs a host's socket
resolves it from the registry by `hostId`. A row's `row` is a host's of
whatever build, so the schema describes it as any object (`forwardedRow`) —
a host's own row is still held to this build's strict `sessionRow` — and a
client reads it tolerantly — the members it knows, the rest ignored — its
`capabilities.rowFacts` saying what it holds, exactly as when the client
polls the host itself.

**The roster's bounds** keep any roster one line, whatever the hosts behind
it send: at most 512 roster rows (`truncated` beyond, in host-id order), and
each roster row at most 32,000 bytes as JSON. The hub keeps every roster row
within them, in this order, and marks each one it changes `approximate`: a
host string over its bound is cut to it; a host row over 16 KiB as JSON is
dropped (`row` absent); and a roster row still over 32,000 bytes — only a
large row beside host strings that JSON escapes can be — has its row
dropped, which always suffices. With a request id of at most 1 KiB, the
hub's `sessions.list` and `sessions.subscribe` replies and every `roster`
notification are then under the 16 MiB line limit (`internal/protocol`'s
`TestAFullRosterFitsOneLine` builds the largest). A host whose ids are not
of the forms above is not listed. The numbers are `internal/protocol`'s
`Roster*` constants, which the schema states too.

**`sessions.subscribe`** (`{}`) is the roster as a subscription, one per
connection (a second is `bad_request`, reason `already_subscribed`). The
reply is the roster at a cursor and the subscription's id, written before any
of its notifications:

```json
{"jsonrpc":"2.0","id":"2","result":{"subscription":"r-1","epoch":"0a1b2c3d4e5f","cursor":9,"sessions":[...]}}
```

then `roster` notifications, each the **net change** since the cursor the
subscriber last had — rows added or changed in `upserts`, each at its
latest; the host ids of rows the subscriber holds that are gone in
`removes`; a host in at most one of the two, and a host that came and went
between two notifications in neither — flushed at most every 250 ms, in
host-id order within each list:

```json
{"jsonrpc":"2.0","method":"roster","params":{"subscription":"r-1","epoch":"0a1b2c3d4e5f","cursor":12,"upserts":[...],"removes":["0190ab12cd36"]}}
```

A subscription ends with the connection, or with a `reset` on the
subscription: `slow_consumer` — a notification's write blocked for 10 s; the
rest of that line is written, then the reset, and the connection stays —
`omitted` — the roster's completeness changed: it went over 512 rows, or
back to 512 or fewer, which a `roster` notification cannot say (`truncated`
is the reply's alone); the connection stays — or `hub_closing` — the hub is
shutting down: it writes the reset within 5 s and closes the connection. A
reset the subscriber does not read within 10 s closes the connection. In
each case the client subscribes again: after `slow_consumer` or `omitted` it
may on the same connection, and reads `truncated` from the new reply; after
`hub_closing`, on a new one, the new hub's `epoch` reseeding it.

On one connection, the lines that carry the roster's cursor — `roster`
notifications and `sessions.list` replies — are written in the order their
roster was taken: a cursor never goes back from one line to the next. A
`sessions.list` reply on a connection that holds a subscription is that
subscription's roster from then on: its next notification is the change from
the rows the reply listed. A `sessions.subscribe` whose reply cannot be
written — a request id long enough to take it over the line limit, answered
`failed`, reason `response_too_large` — subscribes to nothing, and the
connection may subscribe again.

On the hub, every session-scoped method but `session.connect` is
`unsupported`, reason `host_only`: a session's methods are its host's,
reached through the splice.

## Methods

Every session-scoped method carries `sessionId`, the durable craze session
id, at `params.sessionId` (`hello` and `sessions.*` do not). Every mutating
one also carries `commandId`: a canonical positive decimal string, counted
from 1 **per client**, never reused. Resending a mutating method under the
same `commandId` is how a client asks "did that happen?" — see
[Errors and retry](#errors-and-retry).

The hub serves `hello`, `sessions.list`, `sessions.subscribe`,
`session.connect` and `session.create`; every other session-scoped method
sent to it is refused `unsupported`, reason `host_only`. `session.create` is
neither session-scoped (its session does not exist until it answers) nor
mutating (the hub mints no client ids and keeps no command receipts: its
idempotency is its own `requestId`).

| method | mutating | params (beyond `sessionId`/`commandId`) | result | notes |
|---|---|---|---|---|
| `hello` | | see [`hello`](#hello) | see [`hello`](#hello) | tolerant; before any other method; a host's result or [the hub's](#the-hubs-result) |
| `sessions.list` | | — | `epoch`, `cursor`, `sessions[]`; the hub's `truncated?` | a host's one row, or [the hub's roster](#the-hubs-roster) of roster rows |
| `sessions.subscribe` | | — | `subscription`, `epoch`, `cursor`, `sessions[]`, `truncated?` | the hub's: then `roster` notifications ([above](#the-hubs-roster)); a second on one connection is `bad_request`, reason `already_subscribed`; `unsupported` on a host, reason `roster_unsupported` |
| `session.connect` | | `sessionId` | `{}` | the hub's [splice](#the-hub-splice): the connection's first request after `hello`, else `bad_request`, reason `connect_not_first`; `unsupported` on a host, reason `hub_only` |
| `session.attach` | | `cursor?`, `when?`, `budget?` | `subscription`, `session`, `ready`, `after`, `snapshot?`, `reset?` | see [Attach, resume and snapshots](#attach-resume-and-snapshots) |
| `session.detach` | | `subscription` | `{}` | ends this connection's attachment; the reply is its terminal acknowledgement |
| `session.state` | | — | activity, `foreignTurn`, `turn`, `waiting`, `sendNow?`, `queue[]`, `pendingAsks`, `headAsk?`, `err`, `startFailed`, `prompted`, `cancelled`, `settings`, `lastTurn?` | a read, not a cut of the stream; `lastTurn` is [the last turn](#the-last-turn)'s ending |
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
| `session.stop` | ✓ | — | `{}` | served where the session capability `stop` is `true` (every `craze serve`), from any client: a **receipt**, not the stop's completion — see [`session.stop`](#sessionstop); a host whose `stop` is `false` (a TUI-hosted session, an older host) answers `unsupported`, reason `stop_unsupported` |
| `asks.list` | | — | `asks[]` (summaries: `id`, `kind`, `label`, `openedAt`) | |
| `asks.get` | | `askId` | `ask` (the full record, body strings capped at 256 KiB) | |
| `asks.answer` | ✓ | `askId`, `answer` | `{}` | the first valid answer wins; an invalid one is `bad_request`, reason `bad_answer`, and leaves the ask open |
| `session.create` | | `cwd`, `prompt?`, `provider?`, `model?`, `effort?`, `fast?`, `permissionMode?`, `requestId?` (no `sessionId`, no `commandId`) | `session` (a roster row), `prompt`, `promptError?` | the hub's: a new session, answered once it has started — see [`session.create`](#sessioncreate); `unsupported` on a host, reason `hub_only` |

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
  nobody; its client never resends it. A reconnect's fresh `hello` answering
  `resumed: false` means this exact command may have already run under the
  old identity, so resending it under a new one risks running it twice; the
  command instead resolves outcome-unknown (`resume_lost`), and the client
  re-reads state (`session.state`, the attach snapshot) to learn what
  actually happened (`internal/remote/reconnect.go:354-373`).

### `session.stop`

Ends the session and its host — distinct from closing a connection or
detaching a view, neither of which ever stops a session. A host serves it
where its session capability `stop` is `true`; a client that finds `stop:
false` (or no answer but `stop_unsupported`) detaches instead, and the session
runs on where it is.

The reply `{}` is a **receipt**: the host has taken the stop, and the
session's end follows on the stream — its closing records (a running turn's
`ended`, stop reason `closing`; every open ask's `closing`) and then
`reset{session_closed}`, after which the host closes the connection. Before
the receipt is sent, the host has stopped admitting new attachments: from
then on a `session.attach` on any connection is `unavailable`, reason
`closing`, so a client that reads the receipt and attaches is never
half-attached to a session on its way out; an attachment that was already
made (pending, live or closing) goes on to the session's end like any other.

Any client may stop the session, and a stop while one is under way — from
another client, or the same `commandId` resent — is answered `{}` too and
joins the first: the host runs its stop once. A stop runs no command of the
engine's, so it is not in the receipts table: a resend is answered `{}`
again, never `unknown_command`, and a client that lost the receipt need not
resend it at all — it watches for the session's end.

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

### `session.create`

The hub's, where its `hello` says `sessionCreate: true`: a new session in a
host the hub spawns (`craze serve`, as any launch's), answered **once the
session has started**.

```json
{"jsonrpc":"2.0","id":"2","method":"session.create","params":{"cwd":"/home/me/projects/lumen","prompt":"fix the flaky test","provider":"grok","effort":"high","requestId":"new-9c1f04ab7d2e3a10"}}
{"jsonrpc":"2.0","id":"2","result":{"session":{"hostId":"0a1b2c3d4e5f","sessionId":"01a0f89a-4db5-7621-8335-63ae8327352e","host":{...},"status":"reachable","approximate":false,"row":{...}},"prompt":"accepted"}}
```

**Params.** `cwd` is an absolute path to an existing directory. `provider`
is a provider id; absent, it is the hub's configured default — `provider` in
the hub's `config.toml`, read at each create (the provider the last session
to start persisted) — and with none configured the create is `bad_request`.
`model`, `effort` and `fast` are the session's start settings, each absent
for the provider's own default. `permissionMode`, absent, is a plain
launch's: `bypass`, `--force`'s default — `config.toml` has no permission
setting. No params member names an agent binary: the host finds
its own (`[agents]`, then `PATH`), and the hub hands it neither a launch's
`--agent-bin` nor `CRAZE_AGENT_BIN`. `requestId` (1–64 of `[A-Za-z0-9._-]`)
makes the create idempotent (below).

**What the hub does.** It spawns the host with the session's directory as
its working directory and the hub's own environment less what is one
launch's or one terminal's (the environment contract: `CRAZE_HOME` absolute;
no `CRAZE_PROVIDER`, `CRAZE_AGENT_BIN`, `TMUX`, `HERDR_*`, …), reads its ready
line, attaches with `when: "ready"` and waits for the session's start —
at most 60 s — sends `prompt` when there is one (queued, as the session list's
background dispatch sends one, its answer waited for at most 15 s), and lets
go of that connection: a detach after a prompt the session answered, a close
at once after one whose answer was lost or whose deadline passed. Then it
reads the session's own row on a connection of its own, bounded (2 s). The
session runs on in its host, listed in the roster like any other; its host
persists its provider as the next plain launch's default just after its
start, as every new session does — so the create's answer can precede that
save by an instant.

**The result.** `session` is the session's roster row, read fresh from its
host after the create let go of it, and approximate as any roster row's is
(a row the hub had to cut or drop to its bounds says so) — and true also
when that fresh read fails: the create is still a success — the session
exists, and its id must reach the client — and `session` is the row the hub
builds from the registry, `row` absent. `prompt` says what became of the
first prompt:
`none` (there was none), `accepted`, `unknown` (it was sent and its answer
lost — the session exists, and may be working on it; `promptError` says why
it was lost) or `refused` (the session's refusal, in `promptError`; the
session runs on, idle).

**Refusals.** Bad params are `bad_request` (`unknown_field` for a member the
schema does not define). A `cwd` that is no existing directory and a provider
this craze cannot start are `bad_request` too, checked only for a create that
will start a session: a repeat (below) is answered without them. A host that fails to spawn, or to become ready, is
`unavailable`, reason `spawn_failed` — the hub has ended it. A session whose
start fails — its agent binary missing, a locked keychain — or does not end
within 60 s is `not_accepting`, reason `start_failed`, `data.cause` the
host's first error line; the hub stops that host (`session.stop`, then, its
own child, its termination), so nothing of it is left. More than 16 creates
in flight are `unavailable`, reason `busy`; a hub tearing down answers
`unavailable`, reason `closing`.

**Idempotency.** A create's `requestId` is kept with a hash of its
normalized params — in the hub's memory (an answer kept 10 minutes, at most
256 of them; a create in flight never forgotten) and in the new host's
[registry entry](#the-registry) (`requestId`, `requestHash`). A repeat with
the same params is answered the first create's answer — its failure too —
or joins it while it runs; with other params it is `bad_request`, reason
`request_conflict`. A repeat is answered before the directory and the
provider are checked again: a create answered once stays answered, whatever
the configuration or the file system has become. A waiter that disconnects
does not cancel the create. A hub that does not remember the id — it
restarted since — looks among the live hosts of its own namespace (its
`CRAZE_HOME`: a host whose socket is in the hub's own runtime directory; a
create under the same id in another `CRAZE_HOME` is that hub's, and neither
joined nor in conflict). It must prove the id unused before it starts a
session: a registry it cannot read, or a live host whose entry it cannot
read, refuses the create `unavailable`, reason `host_unreachable` — try again
— rather than risk a second session. One whose entry carries the id is
**joined under the same contract**: a session
already started is answered at once (its row, `prompt: "unknown"`: this hub
never saw the prompt's answer, nor sent one); a start still running is
waited for, at most 60 s from the join; a start that failed is answered
`start_failed` and its host stopped. A retry after the created session has
ended finds nothing, and creates another. A client must not retry a create
across a hub restart unless it reuses its `requestId` ([Errors and
retry](#errors-and-retry)).

**Teardown.** A hub that is tearing down gives the creates in flight its own
bound (10 s); past it their waiters see the connection close, and a host
already started runs on, its `requestId` intact for the next hub to join.

On macOS a session the hub creates runs in the hub's security session: a hub
first started over `ssh` cannot start a `cursor` session, whose keychain is
locked there — a start that failed, `start_failed`.

## Notifications

A notification has no `id` and carries its subscription id in `params`: an
attachment's, from a host, or the roster subscription's, from the hub.

| notification | params | meaning |
|---|---|---|
| `event` | `subscription`, `seq`, `event` | one committed event; `event` is the record's body **verbatim** — the same JSON the lossless event codec wrote, never re-encoded |
| `synchronized` | `subscription`, `seq` | the stream has delivered through this attachment's cutoff — shed's "ready" for the *stream* (not to be confused with `ready`, the *session*'s readiness). Sent once per attachment |
| `ready` | `subscription`, `session`, `startFailed`, `err?` | the session's start has finished (`startFailed: false`) or failed (`true`, `err` the text); owed once, only to an attachment made **before** readiness (`when: "now"`, or a `when: "ready"` attach that raced the start) — but not delivered if that subscription resets before its position reaches the seq the start completed at, and not owed at all if the session closes without ever starting (that attachment ends `reset{session_closed}` instead, with no `ready`). `session` is the final [info document](#the-session-info-document) — catalogs and provider session id included |
| `reset` | `subscription`, `reason` | the subscription is over (below) |
| `presence` | `subscription`, `attached` | how many clients are attached to the session now, this one included — see [below](#presence); only from a host whose session capability `presence` is `true` |
| `roster` | `subscription`, `epoch`, `cursor`, `upserts[]`, `removes[]` | the hub's alone: the roster's net change since the cursor the subscriber last had — see [the hub's roster](#the-hubs-roster) |

A `reset` is a notification, not a gap: nothing is ever silently lost. Every
reason and what a client does about it:

| reason | cause | what a client does |
|---|---|---|
| `slow_consumer` | the client fell behind its own budget | after readiness: re-attach **with** its cursor — the log answers from the ring or the journal, or refuses and a snapshot follows. Before readiness: re-attach `when: "ready"`, with **no** cursor |
| `omitted` | a record no client could ever fold (over the per-record limit) | re-attach with **no** cursor — unless this `omitted` is itself the session's closing reset (an unfoldable record in the final tail): the connection then ends exactly as `session_closed` ends it, and there is nothing to re-attach to |
| `replay_failed` | the journal leg of a cursor replay failed asynchronously | discard everything folded since the cursor; re-attach with no cursor |
| `session_replaced` | the host swapped its engine for a new session | the connection closes ([terminal-only from the swap](#the-reply-barrier): no ordinary reply queued before it survives); reconnect, say `hello` afresh (the old token is void) and attach the new session |
| `session_closed` | the session is over; its final records were delivered first — or, if the journal's tail could not be read, the contiguous prefix of them the host still had | the connection closes; there is nothing to reconnect to |
| `hub_closing` | the hub is shutting down — a roster subscription's reset, never an attachment's | the connection closes; reconnect (a hub is started on demand) and subscribe again: the new hub's `epoch` reseeds the roster |

A roster subscription can also end `slow_consumer` (the subscriber fell 10 s
behind) or `omitted` (the roster's completeness changed: it crossed 512 rows,
either way); the client subscribes again and reads `truncated` from the
reply.

### `presence`

A host whose session capability `presence` is `true` — every craze from plan
032 on, detached or TUI-hosted — tells each attachment how many clients are
attached to the session: `attached` counts every attachment the host holds
that is not yet closed (pending, live or closing), less any whose connection's
peer has half-closed, and, on a TUI-hosted session, the hosting TUI's own seat
(so a `craze attach` beside it reads `2`). A connection that only says `hello`
or lists is not attached.

- It is sent on an attachment once its `synchronized` has been written, and
  then whenever the count changes — the latest count only, at most two a
  second on a connection: a count that moves several times between two sends
  goes out once, as its latest value, and one that comes back to what was last
  sent is not sent at all.
- It is never sent before `synchronized`, nor after the attachment's `reset`
  or its `session.detach` reply.
- It has no `seq` and is no session event: nothing sequences, journals or
  replays it, and no cursor covers it. A client keeps the latest for its live
  subscription and forgets it when that subscription ends — until the next
  attachment's arrives.
- Each connection is sent its count by its own writer: a client that has
  stopped reading delays its own count and nobody else's.

A client that does not know `presence` ignores it, as any notification it
does not know. A host without the capability sends none, and its rows carry no
`attached`.

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

The item/byte budget bounds what the log holds for this subscription that
its forwarder has not yet taken: a live record that would take it past
that budget ends the subscription as a slow consumer, and the forwarder
then queues its `reset{slow_consumer}` (a cursor replay already larger
than the budget is refused when the attach subscribes instead). The
forwarder moves records from the subscription into the connection's writer
queue (32 MiB, `protocol.WriterQueueBytes`, every outbound byte counted, of
which 1 KiB is kept for a final reset) and waits only when the next line
would not fit in the rest. So a reader that
simply stops reading is usually reset only once the kernel's socket
buffers and most of that writer queue have filled and the subscription's
budget then overflows behind them — a stalled reader of a modest reply may
never be reset at all. A burst that outruns the forwarder can overflow a
small budget sooner, however full the writer queue is, and a single live
record larger than the budget overflows it at once (the wire fixture
`04-slow-consumer-reattach`'s design).

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
of an in-flight command bounded by what is left of it. That 10 s is the
episode's own clock, not a wall-clock deadline: it pauses while the adopting
connection waits for a `when: "ready"` re-attach's reply (a slow load can
take longer than 10 s) and runs again once that reply arrives or another
write starts, so the episode's total wall time can exceed 10 s while its
clock stays within budget. Past either bound, a client gives up on this
attempt and surfaces the outcome as unknown (`resume_lost` or `disconnected`
— see [Errors](#errors-and-retry)) rather than looping forever against a host
that keeps resetting it, or that it cannot reach at all.

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
| `provider` | `{name, label}` — `name` is the provider id (`cursor`, `grok` or `gx` for the ACP agents, `native` for the one that runs inside craze), `label` what a client shows |
| `catalogs` | `{models[{id,name,recent?}], modes[{id,name,description}]}` — empty until the session is ready. A model's `recent` is its rank in the native provider's model memory: `1` for the model most recently picked in a native session's `/model` that this session offers, then `2`, `3`…; absent for a model not remembered, for every ACP provider's model, and on an older host. A native session's `models` are only those whose provider has a key, plus the one it runs on, judged once when it starts; a client lists the current model first, then the ranked ones by rank, and shows no label for either |
| `capabilities` | the session's own capability set, below |
| `retryHorizon` | `{commands, ageMs}` — the command-id table's size and age bound |
| `permissionMode` | how the host's agent handles permission requests: `bypass` (spawned with `--force`: it runs tools unasked) or `prompt` (`--no-force`: it asks, and a client answers). Absent when the host does not say |
| `startedAt` | when the host started serving the session, UTC — what a client counts the session's elapsed time from. Absent when the host does not say |

`permissionMode` and `startedAt` are absent from the example above, as they
are from every host that does not set them — an older one, or the fake host
by default. Absent, a client falls back to what it knew without them: its own
configuration's permission mode, and an elapsed time counted from its own
attach (see [Versioning](#versioning)).

A native session's catalog carries the ranks (from the fake host's
`18-model-ranks` fixture, which gives its stub a native catalog):

```json
"catalogs":{"models":[{"id":"muse-spark-1.3-contributor","name":"Muse Spark 1.3 Contributor (Meta)","recent":1},{"id":"fireworks/kimi-k3","name":"Kimi K3 (Fireworks)","recent":2},{"id":"fireworks/deepseek-v4p1-flash","name":"DeepSeek V4.1 Flash (Fireworks)"}],"modes":[…]}
```

## Capabilities

Two independent levels: what the **connection** (the endpoint) can do, and
what **this session** can do.

**Connection** (`hello`'s `capabilities`): `rosterSubscribe`, `sessionCreate`,
`multiplex`, `connect` — all `false` on a host in protocol 1, the hub's
alone — plus `snapshot` and `attachWhenNow`, both `true` on a host. On the
hub, `rosterSubscribe` and `connect` are `true`, `sessionCreate` is `true`
where it serves `session.create`, and the other three are `false` (see [the
hub's result](#the-hubs-result)).

**Session** (the info document's `capabilities`): every field of the
engine's own capability set, in its wire name, plus four the protocol states
for every host of protocol 1 — three always `true`, and `stop`, which says
what this host can do — and `rowFacts` and `presence`, the host's too:

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
| `stop` | the host's own, not the provider's: `true` where [`session.stop`](#sessionstop) is served (every `craze serve`), `false` on a TUI-hosted session and on a host from before it existed |
| `rowFacts` | the host's own too, and omitted when `false`: the session's `sessions.list` row carries the [row facts](#the-row-facts). It is a session capability, not a connection one, because it describes the row, and a hub's roster carries rows of hosts of different builds |
| `presence` | the host's own too, and omitted when `false`: the host counts the clients attached to the session, sends each attachment the [`presence`](#presence) notification, and puts `attached` on the session's row |

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
build recent enough to have foo". A capability or a field that is **absent**
means an older host: `capabilities.stop` is `false` there (and `session.stop`
answers `stop_unsupported`), `capabilities.rowFacts` is absent (and so are
the row facts), `capabilities.presence` is absent (and so are the row's
`attached` and every `presence` notification — a client shows no count), and
the info document's `permissionMode` and `startedAt` and
the state's and row's `lastTurn` are simply not there — a
client falls back to what it did before each existed, and the schema, closed
as it is, describes every one of them as optional. A catalog model's `recent`
is optional the same way, except that its absence is ordinary on any host —
the model is not remembered — so a catalog with no ranks at all, an ACP
provider's or an older host's, is listed after the current model in the
client's own order.

The hub is announced the same way. A client learns it is talking to one from
`hello`'s `endpoint.kind`, and what it serves from its connection
capabilities; an older client never dials it — it reaches hosts through the
registry, and refuses a `hello` from anything but a host where it expects
one. The hub lists hosts of every build side by side: each roster row's
`row` is that host's own row, read tolerantly, its `capabilities.rowFacts`
saying what it holds; a host with no `stop` keeps refusing
`stop_unsupported` however it was found. The hub's reasons
(`host_only`, `connect_not_first`, `ambiguous_session`, `already_subscribed`,
`request_conflict`, `spawn_failed`, `host_unreachable`) are new values of the
reason set, each under a code a client already decides from, so one that does
not know a reason decides by its code ([Errors and retry](#errors-and-retry));
the reset reason `hub_closing` ends only a roster subscription, which no older
client holds.

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

## Compaction

A native session compacts its context: it summarizes the conversation so far
and continues from the summary — on its own when the context nears the
model's window, when a request is refused as too large, or on request, when
the user sends `/compact [focus]` (the one command a native session
advertises, and an ordinary prompt on the wire: no method, no capability). No
ACP provider compacts through craze, so none of these events ever comes from
one.

On the event stream a compaction is two `compaction` events, `started` then
`ended`, `{phase, reason, tokensBefore?, tokensAfter?, err?}`, with the
event's `agent` naming the sub-agent whose context it was (absent for the
session's own):

```json
{"type":"compaction","compaction":{"phase":"started","reason":"manual"},"at":"..."}
{"type":"compaction","compaction":{"phase":"ended","reason":"manual","tokensBefore":890000,"tokensAfter":21000},"at":"..."}
{"type":"compaction","agent":"child-1","compaction":{"phase":"ended","reason":"overflow","tokensBefore":300000,"err":"native: provider \"x\" failed (HTTP 500)"},"at":"..."}
```

`reason` is `auto`, `manual` or `overflow` — an **open string**, like the
foreign turn's: an unknown value is still a compaction. An `ended` carries
`tokensBefore`, the context's estimated size before it, and either
`tokensAfter`, its estimated size after, or `err`, why no summary was written.
A `session/load` replay carries only the `ended` of each stored compaction, in
place — with the `/compact …` user event before it for one the user asked
for — stamped `replayed` like the rest of the replay.

Every `started` is followed by its `ended`, whatever becomes of the
compaction. A client folding the stream draws "compacting context…" from the
`started` until the `ended`, and also stops at whatever ends the turn it ran
in — for the session's own, the turn's ending (`done`, `error`, `turn` ended,
a `foreign_turn` ending) or a replay's end; for a child's, that child's
`finished` — so a lost `ended` can never leave it up. The `ended` draws one
note in the transcript, `context compacted · 890k → 21k tokens` (`on request`
for `manual`, `context was too large — compacted` for `overflow`), or
`compaction failed: <err>`.

A snapshot carries an open compaction on the transcript it belongs to — the
main one or a child's — as `compacting {since, reason}`, absent when none is
open, so a client that attaches mid-compaction draws the same "compacting
context…" until the `ended` it folds next. Both fields are additive: a client
that does not know them ignores the kind and the member (see [Tolerant
inbound, strict outbound](#tolerant-inbound-strict-outbound)), and neither
bumps a codec version.

## Usage

A native session reports what it has spent and how full its context is: the
**usage section**, `{contextTokens, contextWindow, turn, session}`. It is a
state delta section like the model or the config — `state.usage` on a `meta`
event — and it stands in the two reads of where things are: `session.state`'s
`settings.usage` and a snapshot's `settings.usage`. No ACP provider reports
usage through craze, so for every ACP session the key is **absent** — never
`null` and never `{}` — as it is for a native session before its first step.
A client reads the key's presence as "this session reports usage".

`settings.usage` is **reported state, not a setting**: it sits in `settings`
because that is where a session's state sections are read, but no client
changes it. It is never a `session.set` kind — those are `model`, `mode` and
`config` — so a client that draws `settings` as controls leaves it out of
them, and shows it as a read-out.

```json
{"type":"meta","state":{"usage":{"contextTokens":68000,"contextWindow":200000,
  "turn":{"input":8000,"output":2000,"reasoning":0,"cacheRead":58000,"cacheCreation":0,"costPicoUsd":71400000000,"unpriced":false},
  "session":{"input":68000,"output":3500,"reasoning":0,"cacheRead":58000,"cacheCreation":0,"costPicoUsd":273900000000,"unpriced":false}}},"at":"..."}
```

- `contextTokens` is the estimated size of what the session's next request
  sends; `contextWindow` is the window of the model it goes to, `0` when craze
  does not know it.
- `turn` is what the turn the report followed spent — after a load, the last
  turn the stored session recorded — and `session` what the whole session has:
  its own requests, its compactions' (a `/compact` is a turn of its own), and
  the sub-agents its turns ran, each priced by its own model.
- Tokens are as the provider billed them: `input` is the uncached input, and
  `reasoning` is part of `output`, never counted twice. `costPicoUsd` is the
  priced part's cost in **picodollars** (10⁻¹² US dollars: `$1` is
  `1000000000000`), exact. `unpriced: true` says some usage in the sum had no
  price in craze's model table: its tokens are counted and it added no cost,
  so the cost is a floor.
- Every key is always present, a `0` and a `false` included.

A report comes after every step, every compaction that ran, and once at the
end of a `session/load` replay. Each carries the whole section — nothing is
summed on the client — so the last one folded is where the session stands. A
load's report rides in the load's install delta (the one stamped `replayed`
before the replay's `end`), never in a delta of its own inside the bracket. A
usage-only delta prints no `craze prompt --json` line, but it takes a `seq`
like every event. The section is additive, like `compaction`: no codec
version bump and no capability.

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
a stored answer. The hub holds no command receipts at all: every refusal of
its own is of that kind.

`aborted` is the one exception worth calling out: the command ran, but its
outcome cannot be vouched for (a caller's context ended mid-flight, a call
that panicked). Re-read state before deciding anything else; never blindly
resend the same *intent* under a new id, or a queued interjection can go in
twice.

Retrying is also bounded by the **connection**: after a reconnect, a client
resends an in-flight command only when the reconnecting `hello` answered
`resumed: true` for the exact client id, token and host it held before —
see [Attach, resume, and snapshots](#attach-resume-and-snapshots).

**A create across a hub restart.** `session.create` (the hub's, where its
`hello` says `sessionCreate: true`) carries a `requestId`, which the hub
stamps into the new host's registry entry, so that a hub that restarts still
knows the session a create started. A client that sees the hub's epoch change
(its `hello`'s `endpoint.hostId` is new) while a create of its own went
unanswered **must not retry that create automatically unless it reuses its
`requestId`**: under the same id the new hub answers with the session the
first create started (or joins its start if it is still running), while a new
id is a new create, and can start a second session.

### The reason table

Every reason a host or the hub sends, grouped by its one code — a reason is
always one code's. Client-side reasons (last group) are named here so a
client never mistakes one for a host's own answer — no host ever sends them.

| code | reasons a host or the hub sends |
|---|---|
| `not_accepting` | `not_accepting`, `not_in_turn`, `start_failed` |
| `aborted` | `command_aborted`, `set_outcome_unknown`, `bad_catalog`, `context` |
| `unavailable` | `log_backed_up`, `ask_unavailable`, `set_unavailable`, `not_run`, `attach_raced`, `not_ready`, `busy`, `closing`; the hub's `spawn_failed`, `host_unreachable` |
| `failed` | `option_gone`, `failed`, `response_too_large`, `snapshot_too_large` |
| `bad_request` | `bad_request`, `bad_answer`, `hello_required`, `unknown_field`, `line_too_long`, `protocol_version`, `bad_token`, `already_attached`; the hub's `connect_not_first`, `ambiguous_session`, `already_subscribed`, `request_conflict` |
| `unsupported` | `unsupported`, `unknown_method`, `stop_unsupported`, `roster_unsupported`, `hub_only`; the hub's `host_only` |
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
| `foreign_turn` | `NotAccepting` | "the session will not take this send-now right now" is exactly `NotAccepting`'s posture, but unlike `not_accepting` and `in_progress`, `protocol.Retry` is **false**: retrying under the same `commandId` after the foreign turn ends replays the stored `foreign_turn` refusal rather than trying again, so a later attempt needs a **new** `commandId` |
| `in_progress` | `NotAccepting` | did not run under a new attempt — it is this command's own attempt, still running; `protocol.Retry` is true (mandatory: it is the safe way to learn the answer). A resend of a command still running reads the same way to a caller: "not yet, try later" |
| `stale_model` | `NotAccepting` | did not run — refused before the provider was ever asked, because the session had already left the model the change was bound to. `protocol.Retry` is true: craze resends the **same** `commandId` once the session is back on that model, judged fresh, never as a replay. This is not an argument error — `NotAccepting`'s "will not take this right now" is the closer shed posture, and it is the closest fit shed has, not an exact one: a shed caller should still consult craze's own `data.code` (and `protocol.Retry`) rather than assume shed's `NotAccepting` alone carries the "resend once the model comes back" rule |
| `unavailable` | `Unavailable` | did not run — a gate refusal; `protocol.Retry` is true, and `LaneError::Unavailable`'s own doc ("nothing to talk to… quiet") already reads as retryable rather than terminal. The hub's `host_unreachable` (a host it could not dial) and `spawn_failed` (a session it could not start) read the same way |
| `unsupported` | `Failed` | reachable by ignoring a capability already `false`, **or** by sending a method protocol 1 does not know at all (`unknown_method`, no capability involved), **or** by sending the hub a session's method (`host_only`: a session's methods are its host's, through the splice); `LaneError` has no dedicated variant for any of them |
| `stale_version` | `BadRequest` | about this command's own arguments (an edit against a row version that moved), the same posture as a malformed request; `protocol.Retry` is false — it ran (was refused) and a resend replays that refusal |
| `stale_turn` | `BadRequest` | a cancel naming a turn that is no longer current — likewise about the command's own arguments; `protocol.Retry` is false |
| `queue_full`, `text_too_long`, `prompt_in_flight`, `prompt_cancelled`, `unknown_row`, `unknown_command`, `unknown_subagent`, `aborted`, `failed`, `index_write` | `Failed` | no `LaneError` variant models a queue, a row, a sub-agent or "ran but the outcome cannot be vouched for"; the craze code's own text, kept in `Failed`'s string, is what a shed client actually shows |
| *(unused)* | `Unauthorized` | protocol 1 has no authentication (`hello.auth` is reserved for S6); no craze code maps to it today |

## The gate table

What each of `internal/engine`'s own gated commands answers in every state
the engine can be in — the table's columns are its mutating methods, every
one but `session.stop`. That one is the host's, not the engine's: it is
mutating and session-scoped, answered `{}` from any client in **every** state
below — closing and closed included — by a host that serves it, and
`stop_unsupported` in every state by one that does not; it is never stored,
so never retried by id ([above](#sessionstop)). It is not
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

Protocol 1 defined the splice before any hub existed, so the hub (S4b)
needed no change to it. A connection to the hub starts in **hub mode**:
`hello` ([the hub's result](#the-hubs-result): no client id, no receipts),
then the roster — `sessions.list`, `sessions.subscribe` — or the splice.

`session.connect{sessionId}` makes the hub dial that session's host and
splice the two connections together: the hub answers `{}` — the last line it
writes on the connection — and from the next byte on, the client is talking
**directly to the host**, starting with its own `hello`. So client ids,
tokens and receipts, always the host's, are minted by the host the splice
lands on, and a resumed connection resumes with that same host, never with
the hub. Bytes the client sent after `session.connect` before the reply — a
pipelined host `hello` — reach the host first, in order.

The splice takes the whole connection, so `session.connect` must be the
**first** request after `hello` — any line the hub has answered since
`hello`, a refused one included, makes it not the first — with nothing else
in flight and no roster subscription; otherwise it is refused `bad_request`,
reason `connect_not_first`, and the connection stays in hub mode. `sessionId`
is matched against every live host's craze session id, provider session id
and host id, as `craze bridge --session` matches it:

| the match | answer |
|---|---|
| exactly one host | `{}`, then the splice |
| none | `unknown_session` |
| more than one | `bad_request`, reason `ambiguous_session` — name the host by its host id |
| one, which the hub cannot dial | `unavailable`, reason `host_unreachable` |
| none known: the hub cannot read its registry | `unavailable`, reason `host_unreachable` |

The hub's dial is a connect and a peer check (the host must run as the same
user), within 500 ms. The hub says nothing to the host: the host sees the hub
as its peer, and `hello`'s `via` stays as the client sent it, absent.

Once spliced, each side's bytes reach the other as they were sent, through
one 32 KiB buffer per direction: a client that stops reading stops the host's
writes toward it, which the host's own write-stall bound (60 s) then ends;
the hub's own writes give up on a peer that takes nothing for 60 s.
Closing is carried through:

- a client that closes its writing half (or its connection) closes the host
  leg's **writing half** only: the host sees the client go — its attachment
  stops counting toward the host's idle exit — and still answers what it
  admitted, which reaches the client;
- the host's end (its connection closed) closes the client's connection;
- any other failure closes both.

A spliced connection counts as one of the hub's clients, keeping it from its
idle exit, until both legs are closed. A hub shutting down half-closes every
splice — each side reads its end after everything already forwarded — and
closes it once it has ended or half a second has passed. A hub that dies
takes its splices with it and nothing else: the session lives on in its host.
The client redials the hub — craze's own clients start one when none
answers, and the new hub finds the session from the registry at once — and
resumes with its token, from its cursor.

A per-session host answers `session.connect` `unsupported`, reason
`hub_only`: a client dialing a host directly is already where the splice
would have put it. The mirror: the hub answers every other session-scoped
method `unsupported`, reason `host_only`.

## The published schema

Every method, notification and shared document has a hand-written [JSON
Schema](https://json-schema.org/) (2020-12) file, embedded in
`internal/protocol` and checked by reflection against the Go wire types both
ways (`TestSchemaCoversEveryWireField`) — a field the code sends that the
schema does not describe, or vice versa, fails the build — the hub's
`session.create` among them, which a host answers `unsupported`, reason
`hub_only`, like `session.connect`. The event and snapshot codecs' own
hand-shaped wire structs get the same treatment in `internal/agent`
(`TestEventSchemaCoversTheCodec`) and `internal/transcript`
(`TestSnapshotSchemaCoversTheCodec`), since this package cannot import
either.

The copy published here, under `reference/protocol/schema/`, is byte-for-byte
the same as the one `internal/protocol` embeds
(`TestPublishedSchemaIsTheEmbedded` fails the build otherwise — it walks the
published copy recursively, so a file or directory added anywhere under
`reference/protocol/schema/`, at any depth, fails the build, not only one
added directly under it). The embedded side (`//go:embed schema/*.json`) is
flat by construction: the glob matches files directly under `schema/` only,
so a subdirectory or a non-`.json` file added there is silently left out of
the embed rather than failing anything — `TestSchemaSourceHasNoNestedFiles`
guards that assumption on the *source* directory itself
(`internal/protocol/schema/`, read straight off disk with `os.ReadDir`, not
through the embed), so a nested or non-`.json` file added there fails the
build too. Every file's
`$id` is `https://charliek.github.io/craze/reference/protocol/schema/<name>`,
so a `$ref` between files resolves at this URL exactly as it does against the
embedded copy — e.g. [`hello.json`](protocol/schema/hello.json),
[`event.json`](protocol/schema/event.json),
[`session.attach.json`](protocol/schema/session.attach.json).

## Fixtures and the fake host

`internal/fakehost/testdata/wire/*.ndjson` is twenty-two scripted scenarios
against a real `internal/control` server over a real engine (wrapping the
TUI's own `Stub`, never a fixture-only re-implementation) — hello and a fresh
attach; a cursor resume and its replay; a foreign-incarnation cursor; a
`slow_consumer` reset and cursor re-attach; an ask answered twice; an invalid
answer; a cancel on idle; a resent `commandId`, replayed and then mismatched;
two clients sharing one prompt; four kinds of refusal; a snapshot of the main
transcript and a child's; an `omitted` reset; a `hello` resume with a right
token, a wrong one, and one aged past its bound; a host that serves
`session.stop`, its info document carrying `permissionMode` and `startedAt`,
answering a stop's receipt and then ending the session; a stop joined by
another client's and by its own resend, with an attach refused `closing`;
and `lastTurn` in `session.state` and a roster row, after a cancelled turn
and a foreign one; a host with the row facts through a turn — a running
tool, streaming text, an ask, the ending; a catalog whose remembered
models carry their `recent` rank; and, against the hub in front of a host,
the hub's `hello` and roster (19), a roster subscription told of the
host's change and then of its leaving the registry (20), and a splice — a
`session.connect` with the client's host `hello` pipelined behind it, an
attach and an event through it, and on a second connection a connect to no
session and one that is no longer the first (21); against a hub that
creates sessions, a `session.create` with a first prompt, taken, its repeat
answered the same, the same `requestId` with other params refused
`request_conflict`, and a create whose session's start fails,
`start_failed` with its cause (22); and a presence host
counting two clients — each told after its `synchronized`, the first told
again of the second's arrival and of its detach, the row saying `attached: 2`
(23). Every line is
`{"conn": N, "dir": "c2s"|"s2c", "msg": {...}}`, plus `{"dir": "op", "op":
{...}}` lines that are not wire messages at all — they script the host
directly (emitting text, opening an ask, restarting the engine into a fresh
incarnation, stalling or dropping connections, running a stop's sequence) —
and, as a fixture's first line or not at all, `{"dir": "host", "host":
{...}}`, which says how the host was built: `stop`, `permissionMode`,
`startedAt`, `rowFacts` and `presence` turn on what an older host does not have, and
`models` gives it a catalog of its own. The seventeen fixtures
without one are, byte for byte, an older host to a newer client. `TestWireFixtures` replays
every one of them byte for byte, validating every line against the schema
above as it sends or reads it — except a c2s line fixture 10 marks
`"invalid": true`: deliberately not a well-formed request of a method
protocol 1 defines with today's params (an unknown method, or a field no
schema allows), sent as it stands and held to no request schema, since it is
designed never to pass one.

A wire line may also carry `"sock": "hub"`: its connection is to the hub's
socket rather than the host's (a connection's first line decides which, for
good). A fixture with any such line is a **two-socket** fixture: the runner
binds and lists its host in a registry of the fixture's own, exactly where a
hub looks for hosts, and starts the hub in front of it — the real hub, in
the runner's process — so one script speaks to both. What the hub writes
carries three values no fixture can pin, named by placeholder as the
incarnation is: the hub's id (its `hello`'s `endpoint.hostId` and its
roster's `epoch`) is `HUB-ID`, its craze version `HUB-VERSION`, and the
runner's own pid — where the hub's `hello` carries it as `endpoint.pid`, and
where a roster row carries it as `host.pid`, the pid the host's registry
entry carries — `999999999`, which no process can have (it is above every
`pid_max`). A pid anywhere else, the fake host's own `4242` in its `hello`
through the splice included, is compared as written. A fixture whose `host`
line says `hubCreates` has a hub that creates sessions, each host it spawns
the test binary run as a fake `craze serve`; a host a create started is
named by placeholder too, from its registry entry: its id (which the hub
minted) as `cccccccccccc`, its session's incarnation as
`CREATED-INCARNATION`, and its pid, in the create's result, as `999999999`.

`cmd/craze-fake-host` is the same host as a standalone binary, for anyone
scripting against protocol 1 without Go: `craze-fake-host --socket PATH`
serves on a socket at `PATH`, listed nowhere; `craze-fake-host --registry
ROOT` instead binds and lists it exactly as a craze host is — its socket in
the runtime tree (`CRAZE_RUNTIME_DIR`), its registry entry and lifetime lock
under `ROOT/.cache/craze/hosts/`, `ROOT` standing for `HOME` — so a hub, or
`craze attach`, given that `HOME` finds it, and unlists it when it exits.
`--host-id` (twelve lowercase hex digits) and `--session-id` replace the
fixed ids, so several can run, and be listed, side by side. It prints one
ready line on stdout,

```json
{"socket":"/tmp/.../host.sock","sessionId":"session-fake-1","hostId":"0123456789ab"}
```

then reads the same NDJSON ops described above from stdin —
`text`, `thought`, `tool`, `permission`, `question`, `plan`, `end`,
`foreign_turn`, `stall_writes`, `resume_writes`, `drop_connections`,
`restart` (a new incarnation of the same session), `quit`, plus a few the
fixtures alone need (`spawn_subagent`, `oversized_event`, `advance_clock`,
`hang_next`, `run_stop`, and `unlist` — a `--registry` host leaving the
registry while it keeps serving the connections it has). Its clock, ids, host id, craze version, pid and token source
are all deterministic by default, so a script against it produces the same
wire traffic on every run and every machine (a listed host's registry entry
carries its real pid, as every host's does).

## Reaching a host

A host's socket lives in a **runtime namespace**, and everything a resolver
needs in order to *find* that socket lives in a separate, fixed place: the
two are split because a Unix socket's path has to fit `sun_path` (about 100
bytes, the way craze binds one — see below), while discovery must work the
same way under an SSH exec as in the terminal tab that started the host,
whatever environment variable either one happens to have set.

### The socket base

A host picks the first usable of four candidates, in this order, each
canonicalised and deduplicated by canonical path (so `$XDG_RUNTIME_DIR`
naming `/run/user/<uid>` makes the second and third candidates the same one):

1. `CRAZE_RUNTIME_DIR`, when set: absolute and validated. Unlike the other
   three, an unusable value here is an **error**, not a fall-through — it was
   asked for by name.
2. `$XDG_RUNTIME_DIR/craze`, when `XDG_RUNTIME_DIR` is set, non-empty and
   absolute.
3. Linux only: `/run/user/<uid>/craze`, when `/run/user/<uid>` exists, is
   owned by the uid, and is `0700`.
4. `/tmp/craze-<uid>` (`/private/tmp/craze-<uid>` on macOS, where `/tmp` is a
   symlink) — never `$TMPDIR`, which on macOS is long enough on its own to
   crowd out `sun_path`.

Only a host searches these bases; a resolver never does; see
[the registry](#the-registry) below.

### The namespace and the socket path

The socket lives at `<base>/<ns>/<hostId>.sock`, where:

- `hostId` is 12 random lowercase hex digits, minted fresh at process start
  and never reused (which is what lets a dead host's own lock file be
  unlinked safely — [Liveness](#liveness-and-the-stale-sweep) below).
- `ns` is the first 8 hex digits of `sha256` over the absolute, cleaned craze
  directory (`CRAZE_HOME`, default `~/.craze`) — one namespace per user and
  per `CRAZE_HOME`, so two craze homes on one machine never share a socket
  directory. A relative `CRAZE_HOME` resolves to one absolute path at
  resolution time; an empty one (no home directory, or the removed
  config-file environment variable still set — see
  [Configuration](configuration.md#the-craze-directory)) is refused before
  anything is built.

A typical path: `/run/user/1000/craze/3f2a9c1e/0190ab12cd34.sock`, about 48
bytes. craze refuses to bind a socket path over 100 bytes — before creating
anything under its base — with an actionable message asking for a shorter
`CRAZE_RUNTIME_DIR`; `sun_path` itself is 104 bytes on Darwin and 108 on
Linux, NUL included, so 100 leaves margin on both.

Both the `<ns>` directory and its parent are `0700`; the socket file itself
is `0600`, chmod-ed to that inside the already-`0700` directory (not bound
under a changed process umask, which is process-wide and would leak to any
other file craze creates concurrently).

### The registry

Everything a resolver must *find* — as opposed to the socket itself — lives
under one fixed per-user path, `<HOME>/.cache/craze/` — the process's own
`$HOME` (`HOME` wins over the account database; **never** `CRAZE_HOME`, and
never affected by `XDG_RUNTIME_DIR` or `CRAZE_RUNTIME_DIR`). An SSH exec is
**assumed** to share this `$HOME` with the tab that started the host —
true for an ordinary SSH login as the same user, not guaranteed in
general — so discovery does not depend on `CRAZE_HOME`, `XDG_RUNTIME_DIR` or
`CRAZE_RUNTIME_DIR`, whatever those held in the launching tab.

```
<HOME>/.cache/craze/
  hosts/<hostId>.json     the registry entry (see below)
  hosts/<hostId>.lock     the host's lifetime flock; holds "<pid> <hostId>"
  locks/<crazeSessionId>.lock   one lock per craze session (SQ16, below)
  host-logs/<hostId>.log        a detached host's log; not part of discovery
  catalogs/<provider>.json      the model catalog cache; not part of discovery
```

A registry entry, `hosts/<hostId>.json`, has exactly these members:

| Member | Holds |
|---|---|
| `protocol` | The control protocol version this host speaks |
| `hostId` | The host's id (the entry's own file name) |
| `pid` | The host process's pid |
| `startedAt` | When the host started |
| `socket` | The control socket's absolute, canonical path |
| `crazeSessionId` | craze's own id for the session |
| `providerSessionId` | The provider's id for the session, `""` before the engine is ready |
| `incarnation` | The engine's incarnation (the journal's own UUIDv7) |
| `provider` | The provider's name |
| `workspace` | The session's working directory |
| `ready` | Whether the engine has started |
| `requestId`, `requestHash` | Only on a host the hub's [`session.create`](#sessioncreate) spawned, and absent otherwise: the create's `requestId` and its params' hash, fixed for the host's life — what a restarted hub joins a retried create by |

This set is a one-way door: a resolver — `craze bridge`, `craze
attach`, shed — reads these members and connects to nothing merely to find a
session, so none of them is ever renamed or removed; members may be added
(`requestId` and `requestHash` were), and a reader ignores those it does not
know.

The socket writes an initial entry at bind time, before any engine is
attached (`crazeSessionId`, `provider` and the rest of the identity still
empty). From there the entry is **rewritten whole**, each rewrite carrying
the engine's complete identity as it stands at that moment, never a partial
update: **queued** the moment an engine is attached to the socket — the one
the TUI starts with, or the one its provider or resume picker builds, a
fresh incarnation each time — and again once that engine becomes ready, when
the provider session id is finally known. Queuing is not landing: the
registry's one writer processes a **current**-engine rewrite in the order
queued, but drops one whose engine has since been superseded — so it may
never be written at all — and until a rewrite lands, or, if it fails, until
the next one for that engine succeeds, the entry can still describe the
previous engine, or, before the first engine is attached, the empty
bind-time entry. A resolver treats the
entry as a pointer to a socket, not as the engine's live identity: `hello`
and `sessions.list`, answered by the socket itself once dialed, are what
actually says which engine is live.

### Liveness and the stale sweep

A host's liveness is its own `hosts/<hostId>.lock` flock, not a connection: a
resolver that finds `hosts/<hostId>.json` opens the matching `.lock` and
tries to take it non-blocking. Held by another process, the host is live and
the entry is read; taken cleanly, the holder is dead and the entry is stale —
**"connect failure is not authority"**: nothing here ever dials the socket
just to decide whether a host is alive.

While holding that lock — the only authority to remove anything of that
host's — a resolver's sweep removes the dead host's registry entry, its
temporaries (writes that died mid-rename before the sweep ever got there) and
last the lock file itself, then unlocks. Host lock files therefore never
accumulate **once a host has a registry entry to be found by**: the sweep
only visits `hosts/<hostId>.lock` names paired with a `hosts/<hostId>.json`
it walked to first, so a host that dies before that first entry exists
leaves a lock nothing here ever opens or removes (SF-49).

**The sweep never removes the socket.** A lock in the cache tree is authority
over that tree's own entry and lock, never over a file in the separate
runtime tree the entry merely *names* — even a hostile copy of the registry,
renamed into place by another writer sharing the same group, carries at most
a copied lock over that copy's own entry, never over a socket somewhere else.
So a crashed host's socket file is left behind in its runtime directory,
under a name that is never reused, until whatever ages that directory clears
it (`/run/user` is a tmpfs emptied at logout; `/tmp` is emptied at boot or by
`systemd-tmpfiles`) — a host's own clean exit still removes its socket,
identity-checked against what it bound. (The residuals this leaves — a
crashed host's temporaries a sweep never visits, and similar edge cases — are
tracked in the project's own follow-up backlog as SF-49.)

### The session lock (SQ16)

`locks/<crazeSessionId>.lock` is a second kind of lock, one per **craze
session** rather than per host: the mechanism behind
[`--continue`/`--resume` of a session already open elsewhere](cli.md#a-session-already-running-in-another-craze).
It holds `"<pid> <hostId>"`, is taken for the life of the process that loaded
or started that session (a detached `craze serve`, or the TUI's own process when detaching is off), and does not depend on the control socket at all —
it is taken even with `control_socket = false`.

## SSH exec: what a client may assume

`craze bridge` is the far side of an SSH transport: something on another
device execs it once per accepted connection (shed's craze lane), and it
resolves and dials the local session for that one connection. What that exec
may — and may not — assume:

- **Nothing from the launching tab's environment.** The exec is a fresh
  process; whatever `XDG_RUNTIME_DIR`, `CRAZE_HOME` or `CRAZE_RUNTIME_DIR` an
  interactive shell happened to have set plays no part in resolving anything.
- **Nothing about the runtime base.** `craze bridge` never searches runtime
  bases itself: it reads the socket's absolute path out of [the
  registry](#the-registry) under `$HOME` — **assumed** to be the same `$HOME`
  as the tab that started the host, true for an ordinary SSH login as the
  same user. A host started under any `XDG_RUNTIME_DIR` or
  `CRAZE_RUNTIME_DIR` is found all the same.
- **How to find the binary at all: a fixed ladder**, modelled on roost's
  `exec_chain_command`
  (`roost-ipc/src/bootstrap.rs:276-453`). One argv element — the whole thing
  is `sh -c '<script>'` — so the far end never has to source a login shell
  just to find `craze`. Each rung is `[ -f "$p" ] && [ -x "$p" ] && exec "$p"
  bridge …` (`[ -f ] && [ -x ]`, never `[ -x ]` alone, since an executable
  *directory* would otherwise pass and then fail the `exec` at 126), tried in
  this order:

  1. `$HOME/.local/bin/craze` — gated on `$HOME` itself being **absolute**
     (`case "${HOME:-}" in /*) …`), the same way rung 2 gates on
     `command -v`'s answer: an unset, empty, or relative `HOME` (`HOME=.`,
     say) must not turn `$HOME/.local/bin/craze` into a path relative to
     whatever directory the shell happened to start in, which `[ -f ] &&
     [ -x ]` alone cannot tell from a real absolute one
  2. `command -v craze`, accepted only when it resolves to an **absolute**
     path (a builtin, function or alias answers with a bare word, which this
     rung refuses — a non-interactive `PATH` essentially never carries a
     relative entry, and exec-ing out of one is a security hazard, not a
     convenience)
  3. `/opt/homebrew/bin/craze`
  4. `/usr/local/bin/craze`
  5. `/home/linuxbrew/.linuxbrew/bin/craze`
  6. `/usr/bin/craze`
  7. `$HOME/.nix-profile/bin/craze` — the same absolute-`HOME` guard as
     rung 1
  8. `/etc/profiles/per-user/$USER/bin/craze`
  9. `/run/current-system/sw/bin/craze`
  10. else: `printf 'craze: command not found\n' >&2; exit 127`

  **The quoting rule.** A craze session id (`--session`'s argument) is
  `[A-Za-z0-9._-]` only, never empty, never `.` or `..` (`ValidToken`,
  `internal/rundir/rundir.go`) — none of those characters can end a shell
  single-quote early, so wrapping the id in one pair of single quotes with
  no embedded quote to escape is always safe. That is a fact about the id,
  not a license to skip quoting it: a client builds this from an id whose
  provenance it does not control, so it must still quote every interpolated
  value going in, the same way roost quotes its whole chain in one pair of
  single quotes. The whole `<ladder>` is itself one more single-quoted
  argument to `sh -c`, so each of those inner `'…'` pairs around the id has
  to close and reopen the outer quoting to survive — written `'"'"'`
  (close the outer single quote, a literal `'` inside a double-quoted pair,
  reopen the outer single quote), not the `'\''` some shells treat as an
  escape and sshd's *login* shell cannot be assumed to (roost's own reason
  for preferring it).

  Here is the exact, complete `sh -c '<ladder>'` argv a client runs, with a
  session id (`01a0bbe5-69b4-79de-b75c-483a15b73d78`) interpolated and
  quoted exactly as above — copy this verbatim and substitute the id:

  ```sh
  sh -c 'case "${HOME:-}" in /*) p="$HOME/.local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"';; esac; p=$(command -v craze 2>/dev/null) || p=; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"';; esac; p="/opt/homebrew/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"'; p="/usr/local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"'; p="/home/linuxbrew/.linuxbrew/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"'; p="/usr/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"'; case "${HOME:-}" in /*) p="$HOME/.nix-profile/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"';; esac; if [ -n "${USER:-}" ]; then p="/etc/profiles/per-user/$USER/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"'; fi; p="/run/current-system/sw/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --session '"'"'01a0bbe5-69b4-79de-b75c-483a15b73d78'"'"'; printf '"'"'%s\n'"'"' '"'"'craze: command not found'"'"' >&2; exit 127'
  ```

  With no `--session` (the one-running-session default), every `bridge …`
  above is plain `bridge`, and the same ladder is:

  ```sh
  sh -c 'case "${HOME:-}" in /*) p="$HOME/.local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge;; esac; p=$(command -v craze 2>/dev/null) || p=; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge;; esac; p="/opt/homebrew/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; p="/usr/local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; p="/home/linuxbrew/.linuxbrew/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; p="/usr/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; case "${HOME:-}" in /*) p="$HOME/.nix-profile/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge;; esac; if [ -n "${USER:-}" ]; then p="/etc/profiles/per-user/$USER/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; fi; p="/run/current-system/sw/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; printf '"'"'%s\n'"'"' '"'"'craze: command not found'"'"' >&2; exit 127'
  ```

  Both forms were verified by running them with `sh -c` the way an SSH exec
  would: a `craze` on any one rung is exec'd with the arguments above, and
  with none the command prints the last line and exits 127.

  craze itself ships no remote-exec code; the ladder above is published here
  for shed (or anything else driving this over SSH) to copy verbatim, so
  "anything the probe can find, the transport can exec" stays a property of
  one list, not two that can drift apart.

- **A per-connection re-exec behind one stable local port is expected and
  cheap**: shed-mobile's model is to `ssh` a fresh `craze bridge` for every
  local connection rather than to multiplex one remote process, and nothing
  about `craze bridge` discourages that.

## Security

The socket is **local, same-uid only**, with no authentication layer above
that in protocol 1 (`hello`'s `auth` field is reserved for scopes, which
arrive at S6) and no encryption, because nothing crosses the machine boundary
here — `craze bridge` is the boundary, and SSH is what encrypts and
authenticates getting to the machine at all.

- **The directories are `0700` and the socket is `0600`.** That stops another
  local user from *opening* the socket by path on this machine, and nothing
  more: an SSH-forwarded Unix socket, for instance, is opened by `sshd`
  itself, running as the forwarding user, so file mode alone says nothing
  about who is really at the other end once a connection is reached some
  other way.
- **So each end also asks the kernel who the other runs as.** A host checks
  every accepted connection's effective uid before reading a byte of it
  (Linux `SO_PEERCRED`, macOS `LOCAL_PEERCRED`/`LOCAL_PEERPID`); `craze
  bridge`, `craze attach`, and `internal/remote` check the uid of
  the host they dialed before writing a byte to it. A lookup failure is
  always a refusal, never an allow — a connection whose owner cannot be
  named is exactly the one not to trust. Any OS other than Linux or macOS
  refuses every connection outright, for lack of a way to ask.
- **The cache tree — the registry, host locks, and session locks — is held
  by file descriptor, not trusted by path.** It is walked from `/` one
  component at a time with `openat(O_NOFOLLOW)`, and everything below its
  leaves — a lock file, the registry's own rename, a stale entry's removal —
  is done relative to the descriptor that walk returned, never by re-opening
  a path. That is what makes a group-writable `~/.cache` (a user-private
  group under a permissive umask, common enough on real machines) safe to
  accept there without reading any system file to prove it exclusive: the
  worst a member of that group can do is rename something into place after
  validation, which — because everything from the leaf down runs against the
  held descriptor — makes the *next* walk refuse, not this one misbehave.
  The separate runtime tree (where the socket itself lives) has no such
  exemption, because `bind(2)` takes a path and nothing holds it open between
  the check and the call: a group-writable ancestor there is refused
  whoever owns it.
- **Scopes are S6's.** Protocol 1 has none: same-uid access is all-or-nothing
  for now, and `hello`'s reserved `auth` field is where a future scope
  narrows it.
