# 06 — shed lane (S3)

> **Design settled 2026-10-03; the brief is shed's
> [`docs/discovery/craze-lane.md`](https://github.com/charliek/shed/blob/main/docs/discovery/craze-lane.md)
> (SD-40 to SD-46).** It changes this page in these places, and wins where they
> differ:
>
> - **The hub is a machine-level source, not a lane hung off a roost tab
>   (SD-42).** One `craze bridge --hub` connection per machine lists every
>   craze session, tab-hosted or headless, says which providers can start, and
>   creates sessions; each open transcript is a connection of its own. shed's
>   contract splits into a machine-level source and a session-level lane with
>   per-session capabilities.
> - **Status is craze's for craze sessions (SD-43, replacing SD-15 for
>   them).** A roost tab craze owns folds into the hub row it names.
>   **Correction:** roost's ownership key is the *provider* session id
>   (`internal/tui/host.go`, as SQ5 says), not craze's own; the row's
>   `providerSessionId` matches it.
> - **craze is shed's only ACP lane.** gx's lane and shed's
>   cursor/codex/grok/gx kinds are retired from shed (SD-40), so "the third
>   lane after opencode and gx" below is history.
> - **Create picks a provider; settings change in the session.** A provider
>   availability check and a hub create-options call come first, in craze
>   (S3a, SD-44, SD-46). A running session's model, effort, fast mode, context
>   and mode change from one settings sheet (SD-45).
> - **S3a shipped (Plan 036, PR #87).** The hub answers
>   `sessions.createOptions` (providers with `ready` / `needs_setup` /
>   `unavailable`, a reason and a fix, the default provider, recent
>   directories), advertised by the `createOptions` connection capability, and
>   `craze providers [--hub] [--json]` prints the same. shed's tests can run
>   the real hub against fake hosts and a fake agent with the verified
>   hermetic recipe (`docs/reference/protocol.md`, "Testing a client against a
>   real hub", and `tests/cli/test_client_recipe.py`). S3b, in shed, is next;
>   the handoff is in `12`, Plan 036.
> - **Item 3 below is corrected.** `shed-host-agent` is shed's credential
>   broker and runs nothing for lanes. Cursor from the phone on a Mac hub
>   started over ssh is dimmed for the MVP, and the fix is a follow-up
>   (SF-126, SD-44).
> - **Order (SD-46).** S3a (craze) runs first, then S3b (shed and
>   shed-mobile). shed does not release first.
>
> The rest of this page stands as the protocol mapping and the rules shed
> binds craze to (FRB mirror, two signals, `option_for`, append-only rows).

**Order (SD-34, superseded by SD-46):** S3 runs after S4b (the hub) and after
shed has made its first release of its own lane work, so it starts with a hub
roster and `session.create` (shed's `create` capability), both brought by S4b.

How craze becomes the third full agent lane in shed and shed-mobile, after
`opencode` and `gx`. The work is mostly **in the shed repo**; craze's side is
S2's socket and `craze bridge`.

## Where craze stands today

craze is already a status-only agent for shed-mobile, for free, through roost:
it reports `tab.agent_report` with source `craze`
(`internal/host/roost.go`; roost's `docs/guides/agents.md` lists craze as the
sixth supported agent and the first needing no hook install). On the phone
that is a machine row with an activity chip and a read-only `tab.dump` peek,
under the unknown-kind policy (`RcKind::Other("craze")`). No transcript, no
input.

Neither shed nor shed-mobile mentions craze anywhere yet.

## The contract to satisfy

`shed_core::lane::AgentLane` (`shed/crates/shed-core/src/lane.rs`, the module
doc is the spec) is ten methods: `capabilities`, `sessions`, `session`,
`history`, `create`, `send`, `cancel`, `approvals`, `answer`, `subscribe`.
Dispatch is by the row's `kind` string; adding craze means a `shed-craze`
crate and one more entry in shed-mobile's `LANE_KINDS`
(`shed-mobile/rust/src/api/lane.rs`).

Generic and free to a new adapter: every DTO, the `Reset … Ready … Down`
bracket, the bounded ring and backoff, `option_for`, the whole client UI.
Per-agent: the transport, the reconnect loop, the fold from the agent's wire
format into `RcFeedMessage` rows, the error mapping, and
credentials/discovery if the agent needs them.

**The shared fold/view is free only if the adapter feeds it append-only
rows** (SD-29). `RcFeedMessage` has a sequence but no stable entry id, and
`LaneView::apply` / `Snapshot::push` append every message, so an entry upsert
would duplicate. craze's log is per event (`03`), and `shed-craze` segments it
into append-only rows the way shed-gx does (8 KiB / 2 s / kind change / turn
end). No upsert crosses into shed's DTOs; journal cursors stay distinct from
shed's feed-row sequences.

| `AgentLane` | craze protocol (`05`) |
|---|---|
| `capabilities` | the **session's** capabilities (interject, create, cancel, approvals, history cursor). The trait's `capabilities()` is synchronous and static for the adapter's life; adapters are built per session, so the adapter fetches them before it is constructed |
| `sessions` / `session` | `sessions.list` |
| `history(cursor, limit)` | a **bounded page** with `truncated`, not an open-ended attach: S2's history call takes `limit`, `maxBytes`, and `beforeSeq`. Cheap now, hard later, because shed-mobile pins by git rev |
| `subscribe(cursor)` | `session.attach{cursor}`, left open |
| `send(mode)` | `session.prompt{mode}` |
| `cancel` | `session.cancel`; idle refusal maps to `NotAccepting` (shed's correction 8) |
| `approvals` / `answer` | `asks.list` / `asks.get` / `asks.answer` |
| `create` | `session.create` (needs the hub, S4b; capability false until then, and available when S3 starts) |

Target capabilities: `interject` per provider, `cancel`, `approvals`, and
**`history_cursor: true`**, the capability the phone rewards most (silent
reconnect, no reseed, no flicker after backgrounding).

## What is different from gx, on purpose

| gx lane | craze lane |
|---|---|
| Loopback HTTP + SSE | One NDJSON stream over a Unix socket |
| Bearer token file with strict file discipline | None; the socket's uid boundary (SD-04) |
| Discovery record + `healthz`/`instanceId` pinning | None; fixed paths, no ports to recycle |
| SSH probe script to read the token | None |
| `gx.remote` roost metadata hint, stale and unclearable | None needed: roost already carries craze's session id as the ownership key, and `craze bridge --session <id>` resolves it |
| `ssh -L`-style forward per lane plus a probe exec | SSH **exec** of `craze bridge` behind the device's stable local port, the mechanism shed-mobile already uses for roost (`lib/ssh/roost_tunnel.dart`) |
| Approvals and roster not resumable; re-fetch + tombstones per reconnect | Ask transitions are sequenced events and resume exactly; the roster has its own epoch and cursor |
| Cancel is a fire-and-forget notification | Acknowledged request |
| Transcript root-only; a session blocked on a child's approval shows no reason | Sub-agent events carry the agent id; child transcripts are attachable |
| Lane exists only while a leader exists | Detached hosts (S4a) |
| Lane is a sibling observer of the leader | craze **is** the process driving the agent, which is the position shed's rule asks for: "drive the session's own server, never a sidecar" |

## Rules from shed that bind craze's design

- **FRB-mirror rule.** Every DTO crossing into shed-mobile is an owned
  `String` / `Option` / `Vec` / scalar. No maps, no `serde_json::Value`.
  Free-form payloads ride as a string holding raw JSON. Break this and the
  adapter compiles on desktop and silently strands the phone.
- **Two independent signals.** `activity` answers "is it running";
  `pending_approvals` (a count) answers "is it blocked on me". The roster must
  carry both; a client keying its badge off activity alone misses approvals.
- **`option_for` refuses to guess.** A live gx leader offered two options of
  kind `allow_once`, one of which turned prompts off for the session. craze
  passes through what its provider offers, and grok does exactly this
  (`agent.OptionIDForKind` exists for it), so craze **cannot** promise unique
  kinds. It preserves every offered id and kind verbatim and always accepts
  the exact offered id; a by-kind shortcut that is ambiguous is refused.
- **One status authority per session.** For a tab-hosted session that is
  roost (SD-15). The lane adds transcript and control; it does not compete on
  status. Headless sessions (S4a) have no tab, so their roster comes from the
  hub (S4b); how shed lists those is SQ8.
- **No pane scraping**, ever.

## Work on the shed side

1. `crates/shed-craze`: NDJSON client, reconnect loop, segmentation and fold
   into `RcFeedMessage` (shed-gx's fold is about 1,400 lines; its 800 lines of
   discovery and most of its credential handling have no counterpart here),
   error mapping. Rough size: 3k lines of Rust.
2. **The transport handoff, stated exactly (SD-29).** On the phone Rust never
   runs an SSH exec: Dart owns the SSH connection, exposes a **stable local
   port**, and re-execs the remote command for each accepted connection; Rust
   reconnects through that port (`RoostTunnel`, `BridgeLaneSpec`). An SSH
   handle cannot cross FRB. So the device keeps its bounded stable-port pump
   and only the remote endpoint changes to `craze bridge`; for Rust the lane is
   a TCP dial speaking NDJSON, which is a smaller change to `shed-core` than
   a new stream type. The host stays Unix-socket-only. To specify: who owns
   SSH re-exec, half-close, teardown, and how a desktop reaches a session on
   **another** machine (the same pump; a direct Unix-socket dial covers only
   local sessions) (SQ8).
   **Accepted risk:** the stream is re-exposed on the device's loopback with
   no peer identity, as roost's already is. "No tokens" is a property of the
   machine running craze, not of the client device.
   **Known residual (SF-139):** `craze bridge` and the hub's splice now end
   within about a second of a client that has closed entirely, once the
   bridge has seen its stdin's end and the splice its client's (Plan 035; a
   killed ssh client over Tailscale SSH was reaped in about 1 s; a client that
   closes while the splice's upstream copy is blocked on a host that stopped
   reading is reaped by the 60 s write-stall bound instead), and a client
   that half-closes still receives. But a client that closes its stdin and
   *then* drops, over Tailscale SSH, leaves the bridge and splice until the
   host's next write (tailscaled keeps its end of the bridge's stdout pipe
   open, so nothing shows the client has gone). It holds the hub from its idle
   exit, not the host; the lane's reconnect loop should expect it.
3. macOS limit to record in shed's `create` capability: `cursor-agent` started
   under plain ssh fails on a locked login keychain, so `session.create` over
   the bridge cannot start cursor sessions there. A hub keeps the login session
   of whichever command first started it, and every session it creates
   inherits it (SF-126), so **shed's lane should run `craze bridge` through a
   GUI-session process such as `shed-host-agent`** (a LaunchAgent in the GUI
   domain), so that a bridge never gives birth to an ssh-session hub;
   otherwise the limit applies only while no GUI-born hub is running. A create
   that fails this way says so (plan 035): its `data.cause` carries cursor's
   own lines and a hint naming the hub's pid to stop.
4. `RcKind` gains `craze`; shed-mobile's `LANE_KINDS` gains it; fake host
   fixtures join `shedtest` so the mobile integration cells run hermetically.
5. Acceptance is shed's bar: start craze in a roost tab, then from the Flutter
   desktop build and the phone read the transcript, send a prompt, cancel,
   answer a permission and a question.

## Resuming a saved session (plan 046 B10; shed's work, not done in craze)

craze's hub serves `sessions.saved` (the saved sessions, each with the id that
reopens it) and `session.reopen` ("ensure this saved session is running")
from plan 046 on; its `hello` says so with the connection capability
`reopen`. Closes craze#88 on craze's side. To adopt it, shed:

1. Reads `reopen` in `ConnCapabilities` (`wire.rs`), beside `createOptions`,
   and hides the resume entry when it is absent: an older hub refuses both
   methods (`unsupported`, or `unknown_method` from one before them).
2. Adds `AgentSource::saved(workspace?)` (`sessions.saved {workspace?}`; its
   `limit` 1–200, default 50; sub-agents' sessions left out unless
   `subAgents`) and `AgentSource::reopen(id)` (`session.reopen {id}`), routed
   through the eager, user-action dial, the same as create: a listing needs a
   hub, so it may start one. The `id` is opaque: send it back exactly as
   `sessions.saved` gave it (a craze id, or `<provider>:<providerSessionId>`
   for a row from before craze ids).
3. Opens **one short connection per reopen**, as it does per create: a hub
   answers a connection's requests in order, and a reopen holds its
   connection until its answer — up to 180 s.
4. Allows a deadline of **at least 200 s** for the answer (the hub's own bound
   is 180 s; the start alone may take 150 s, the agent's load 90 s of it).
5. Binds `open` to the answer row's **host id** (`session.hostId`), and
   connects there (`session.connect`). An approximate answer row (its fresh
   read failed) is treated as create's held row: the session exists; read it
   again from the roster.
6. Classifies outcomes by `data.code` and `data.reason`:

   | Outcome | What the client does |
   |---|---|
   | Connection lost, deadline passed, or failure to redial (an unknown outcome) | Resend the same `{id}` on a fresh connection, once. A second unknown is shown as unknown. |
   | `unavailable/busy` or `unavailable/closing` | Transient. Offer retry, no automatic loop. |
   | `unavailable/host_unreachable` or `unavailable/spawn_failed` | Show the message. |
   | `unknown_session` or `not_accepting/not_resumable` | Final. Drop the row from the saved list. |
   | `not_accepting/start_failed` | Final. Show the cause (`data.cause`). |
   | A malformed success | Treat as unknown. |

   A resend is safe: the hub's retry safety is the session's claim (one
   host serves a session, and every reopen joins it — a TUI-hosted session
   whose claim could not take its lock file is the exception, and runs
   unclaimed), not a request id.
   The host id and `joined` may differ across tries, and a try sent after the
   session was stopped reopens it again: the client stops retrying when its
   user cancels.
7. Extends its hermetic recipe cells (`crates/shed-craze/src/testing.rs`):
   seed `sessions.jsonl` in the recipe's `CRAZE_HOME`, run the fake agent
   with `CRAZE_FAKE_SCRIPT=grok-load`, and reopen through the real hub —
   craze's own cell is `tests/cli/test_client_recipe.py`'s
   `test_a_client_reopens_a_saved_session`, written out step by step in
   `docs/reference/protocol.md`'s "Reopening a saved session in the recipe".

shed's docs then drop the limits they name: "a client resume needs a hub call
of its own" (`docs/discovery/craze-lane.md`) and "a saved session cannot be
resumed from the desktop (craze#88)" (`docs/desktop/agent-lanes.md`). The
phone takes it through the pinned shed rev.
