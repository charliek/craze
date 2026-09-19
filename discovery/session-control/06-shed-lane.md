# 06 — shed lane (S3)

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
| `create` | `session.create` (needs the hub, S4; capability false until then) |

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
| Lane exists only while a leader exists | Headless hosts (S4) |
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
  status. Headless sessions (S4) have no tab, so their roster comes from the
  hub; how shed lists those is SQ8.
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
3. macOS limit to record in shed's `create` capability: `cursor-agent` started
   under plain ssh fails on a locked login keychain, so `session.create` over
   the bridge cannot start cursor sessions there.
4. `RcKind` gains `craze`; shed-mobile's `LANE_KINDS` gains it; fake host
   fixtures join `shedtest` so the mobile integration cells run hermetically.
5. Acceptance is shed's bar: start craze in a roost tab, then from the Flutter
   desktop build and the phone read the transcript, send a prompt, cancel,
   answer a permission and a question.
