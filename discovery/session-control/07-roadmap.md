# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one to three
PRs, gated per commit. S1–S5 are committed; S6 and S7 are directional and get
decided after S1–S5 are in daily use (SD-12). Sizes are rough estimates of
non-test source lines, from the reference reviews (`09`).

| ID | status | one line |
|---|---|---|
| S0 | complete | Discovery: four codebases reviewed, topology / protocol / remote scope / journaling settled |
| S1 | not started | Engine core: fan-out, sequenced journal, engine-owned asks and turn state, render-free transcript; TUI becomes the first client |
| S2 | not started | Per-session Unix socket, published protocol spec + schema, fake host, `craze bridge`, `craze attach` |
| S3 | not started | `shed-craze` lane adapter in shed; craze in shed-mobile's `LANE_KINDS` |
| S4 | not started | Headless session hosts, per-machine hub, `craze serve` / `craze ps`, detach |
| S5 | not started | Agent view in the TUI |
| S6 | directional | `craze web`: hub serves WebSocket + a web bundle on loopback / tailnet |
| S7 | directional | Outbound relay uplink and a hosted server; enrollment, scopes, TLS |

## Phase detail

### S0 — discovery

**Exit result:** completed 2026-09-19. Findings in `01`–`06`, decisions
SD-01 to SD-17, open questions SQ1–SQ12, harness overlap in `11`.

### S1 — engine core

Detail in `03` and `04`. Three PRs: S1a broker + sequence numbers + journal
writer; S1b ask registry, turn state, acknowledged cancel, command ids, index
writes; S1c render-free transcript and in-process snapshot/`afterSeq` attach.

- Size: L, about 3–4k lines plus test churn. S1c is the risk: frame goldens
  and `transcript_test.go` must not move.
- Ordering against the harness: does not block H2; should precede any harness
  phase that introduces an ask channel, sub-agent event plumbing, or resume
  (`11`).
- **Exit**: the TUI is byte-identical on the existing goldens while consuming
  a subscription; a second in-process subscriber attached mid-turn with
  `afterSeq` reproduces the first's transcript exactly; every session,
  ACP and native, leaves a journal that replays to the same transcript;
  `craze prompt --json` lines carry `seq`; an ask answered twice yields one
  resolution and one `already_resolved`.

### S2 — control socket

Detail in `05`. The protocol package, schema, socket server in the host,
`craze bridge`, a minimal `craze attach` (a second TUI on a running session,
which is also the best test client), the fake host, published reference docs.

- Size: M, about 2–3k lines.
- **Exit**: two TUIs on one live `cursor-agent` and one `grok` session show
  the same transcript; a prompt from either appears in both; an ask answered
  in one closes in the other; kill and reattach resumes silently from
  `afterSeq`; a deliberately stalled client is reset as `slow_consumer`
  without delaying the agent; socket refused for another uid; live smoke on
  Linux and the mac-mini.

### S3 — shed lane

Detail in `06`. Work is in the shed and shed-mobile repos and follows their
process ("mobile first").

- Size: M, about 3k lines of Rust plus the lane-transport change in
  `shed-core`.
- **Exit**: shed's own bar. craze started in a roost tab; from the Flutter
  desktop build and the phone: read the transcript, send a prompt, cancel,
  answer a permission and a question; background the phone for a minute and
  resume with no reseed.

### S4 — headless hosts and the hub

`craze serve` (a host with no TUI), detach from a running TUI without ending
the session (SQ12), the hub at `run/hub.sock` with roster, routing, spawn,
and stop; `craze ps`; `craze attach <id>`; `session.create` turns shed's
`create` capability on.

- Size: M, about 2k lines. prox's lesson: every hard bug here is a
  **lifecycle** bug (leaked registrations, late close callbacks racing a
  reconnect, shutdown order). Write generation-guarded registration and
  "cancel workers, join, then deregister" teardown on day one.
- Reuse from prox: flock'd pidfile singleton, auto-spawn by re-exec with an
  env marker, health poll, exit-when-idle, stale sweep keyed on pid plus a
  start token.
- **Exit**: start two headless sessions, close every terminal, list them with
  `craze ps`, attach to one; kill the hub mid-turn and lose nothing, hosts
  re-register with the respawned hub; a host crash removes its row within a
  sweep; hub and host of different craze versions interoperate on the
  protocol integer.

### S5 — agent view

A TUI screen that is a hub client: rows with provider, title, cwd, activity,
pending ask count, last change; open a session in place; answer an ask from
the list; start a new headless session. The existing sub-agent view is the
UI precedent.

- Size: M, about 2–3k lines.
- **Exit**: the `01` demonstration, second half: two headless sessions found
  after reopening craze, one blocked on an ask and answered from the view.

### S6 — `craze web` (directional)

The hub serves the protocol over WebSocket and a static React bundle, bound
to loopback or a tailnet interface only (`02`). No accounts, no relay.

- Size: L; the web client is most of it.
- Decide after S1–S5: if shed-mobile and shed desktop cover the need, this
  may never be built.

### S7 — relay (directional)

The hub dials out; a hosted server republishes the protocol to web and mobile
clients off the tailnet. Needs machine enrollment, scopes, TLS, a craze-side
capability allow-list, and visible remote-attach state (`02`).

- Size: XL; it is a product, not a feature.
- Its one clear benefit over S3 + tailnet is reach when off the tailnet.

## Deferred (not scheduled)

`craze journal` mining subcommands · remote PTY · port forwards and preview ·
file browse/edit over the protocol · yamux transport · persisted command
receipts · windowed snapshot pagination · multi-user anything · a non-Go
server · IDE-like desktop shell.

## Conventions per phase

- Plan first (panel review), then the repository's gated implementation flow;
  `make lint && make test && make test-race && make build && make test-cli`
  per commit.
- Any phase that changes the wire updates the schema, the fixtures, and the
  published spec in the same PR.
- Every phase closes with a live smoke on Linux and the mac-mini against real
  agents, recorded as an **Exit result** here.
- Update `08` when a phase changes a decision; update the table above and the
  README status table when a phase merges.
