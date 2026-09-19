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
bracket, the fold/view (`shed-app/src/lane_view.rs`), the bounded ring and
backoff, `option_for`, the whole client UI. Per-agent: the transport, the
reconnect loop, the fold from the agent's wire format into `RcFeedMessage`
rows, the error mapping, and credentials/discovery if the agent needs them.

| `AgentLane` | craze protocol (`05`) |
|---|---|
| `capabilities` | `hello` reply (`Capabilities`: interject, create, cancel, approvals, history cursor) |
| `sessions` / `session` | `sessions.list` |
| `history(cursor, limit)` | `session.attach{afterSeq}` replay; cursors are `seq` |
| `subscribe(cursor)` | the same attach, left open |
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
| `ssh -L`-style forward per lane plus a probe exec | One SSH **exec** of `craze bridge`, the mechanism shed-mobile already uses for roost (`lib/ssh/roost_tunnel.dart`) |
| Approvals and roster not resumable; re-fetch + tombstones per reconnect | Sequenced in the journal; resume is exact |
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
- **`option_for` refuses to guess.** gx once offered two options of kind
  `allow_once`, one of which turned prompts off for the session. craze should
  never offer two options with the same kind, and always round-trips the
  exact offered id.
- **One status authority per session.** For a tab-hosted session that is
  roost (SD-15). The lane adds transcript and control; it does not compete on
  status. Headless sessions (S4) have no tab, so their roster comes from the
  hub; how shed lists those is SQ8.
- **No pane scraping**, ever.

## Work on the shed side

1. `crates/shed-craze`: transport (exec bridge → NDJSON), reconnect loop, fold
   into `RcFeedMessage` (shed-gx's fold is about 1,400 lines; its 800 lines of
   discovery and most of its credential handling have no counterpart here),
   error mapping. Rough size: 3k lines of Rust.
2. A lane transport that is a **duplex stream from an SSH exec** rather than
   an HTTP base URL. Both existing lanes assume loopback HTTP
   (`agent_lane()` validates a `loopback_base_url`), so this is a real change
   to `shed-core`'s lane wiring, and the desktop's local dial is a direct
   Unix-socket connect (SQ8).
3. `RcKind` gains `craze`; shed-mobile's `LANE_KINDS` gains it; fake host
   fixtures join `shedtest` so the mobile integration cells run hermetically.
4. Acceptance is shed's bar: start craze in a roost tab, then from the Flutter
   desktop build and the phone read the transcript, send a prompt, cancel,
   answer a permission and a question.
