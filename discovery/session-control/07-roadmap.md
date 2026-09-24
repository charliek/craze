# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one to three
PRs, gated per commit. S1–S5 are committed; S6 and S7 are directional and get
decided after S1–S5 are in daily use (SD-12). Sizes are rough estimates of
non-test source lines, from the reference reviews (`09`).

| ID | status | one line |
|---|---|---|
| S0 | complete | Discovery: four codebases reviewed, topology / protocol / remote scope / journaling settled |
| S1 | complete | Engine core: fan-out, sequenced journal, engine-owned asks and turn state, render-free transcript; TUI becomes the first client. **S1a complete (2026-09-19)**; **S1b complete (Plan 021, all three PRs merged, 2026-09-21)**; **S1c complete (Plan 024, PRs #50 and #52, 2026-09-24)** |
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

Detail in `03` and `04`. Three slices, each its own plan and PR:

- **S1a** (complete) — the ordering boundary and `Subscribe` (primary and
  budgeted subscribers) as one component shared by the ACP session, the native
  adapter, and `tui.Stub`; sequence numbers; incarnation identity; the
  lossless event codec; the ring; the journal writer; `seq` on
  `craze prompt --json`. No behavior change.
- **S1b** (complete) — the engine turn driver (admission, queue drain,
  send-now, foreign turn wait, synthetic endings and rows); the
  shared/client-local split of TUI-authored rows and state deltas in place of
  bare `EventMeta`; ask registry with sequenced terminal outcomes; cancel
  outcomes on an engine turn id; command ids; settings order; approval policy
  separated from frontend presence; durable craze session id and index writes
  in the engine. **Shipped under Plan 021 (panel-reviewed 2026-09-20): three
  PRs — the driver, the asks, state and identity.**
- **S1c** (complete) — the render-free `internal/transcript` model folded
  inside the boundary (the engine's own instance, plus one per client); a
  bounded, lossless snapshot codec; in-process attach (`Engine.Attach` on
  `Control`) with snapshot + cursor and three pinned error paths; a
  convergence check per event kind. The TUI became its first client with no
  golden moved except four permitted one-row changes: SF-01's two title rows
  (X38) and X39's two spinner glyphs. **Shipped under Plan 024: two PRs —
  the package/engine/attach (#50, `27c1db6`), then the TUI (#52, merged
  2026-09-24).**

- Size: L, about 4–5k lines plus test churn (raised after the panel: S1b is a
  driver, not a field move). S1b and S1c are the risk.
- Ordering against the harness: does not block H2; S1b should precede the
  first harness phase that adds an ask channel, and H6 and H7 (`11`). Harness
  D-39, on `main` since 2026-09-19, moved permission prompts out of H3, so
  that is no longer the first such phase.
- **Exit (S1a)**: goldens and `craze prompt --json` output unchanged apart
  from `seq`; a budgeted subscriber attached mid-turn with a cursor receives
  exactly the events the primary did after it, in order, across the
  ring/journal seam; a stalled budgeted subscriber is dropped without delaying
  the primary; every session, ACP and native, leaves a journal whose `event`
  lines decode losslessly to the emitted events; a failing or stalled disk
  degrades to a recorded gap, never a stalled turn; race detector clean.
- **Exit (S1b)**: two in-process clients on one session cannot double-drain
  the queue; a session with no client drains its own queue and parks asks; an
  ask answered twice yields one resolution and one `already_resolved`, and an
  invalid answer leaves it open; every ask ending is a sequenced event; a
  cancel names its turn and cannot hit the next one; two concurrent settings
  changes converge on every client; golden files byte-identical; and, because
  `craze prompt`'s own driver moves into the engine, its `--json` fixtures and
  tests are byte-identical apart from `seq`.
- **Exit (S1c)**: golden files byte-identical, with four permitted one-row
  exceptions (SF-01's two title rows, X38; X39's two spinner glyphs), and
  `transcript_test.go` assertions move packages unchanged; a second
  in-process subscriber attached mid-turn from a snapshot reproduces the
  first's transcript exactly; snapshot and replay memory stay inside stated
  byte bounds on a worst-case session.

**Exit result (S1a):** completed 2026-09-19, plan `020-session-control-s1a-event-log`,
branch `feature/plan-020-session-control-s1a` (the PR number goes here on
merge). All six exit clauses met,
each against a named test or the Linux smoke; the criterion-by-criterion table,
the amendments X1–X18, the Linux smoke record, and the V3/V4 measurements are in
`12`. Two qualifications, both recorded there rather than waved through: the
live smoke could not exercise a **native tool call** (H1 has no tools; re-run
after H2 lands), and V6's "`config.toml` byte-identical" clause **fails for a
pre-existing reason** — `persistProvider` writes the `provider` key on every
run, unchanged on `origin/main` — so it is restated as "unchanged apart from
the `provider` key". The mac-mini half ran at the branch tip and passed for
grok and native; `cursor-agent` is skipped there because the login keychain
blocks it over ssh, and that skip exercised the stderr tee against a real
agent. Inputs to SQ3 (retention) and SQ15 (tool-event conflation) are
measured in `12` and left undecided.

**Exit result (S1b):** shipped 2026-09-21, plan `021-session-control-s1b-engine`,
three PRs from fresh `origin/main` — `feature/plan-021-s1b-driver` (#41,
`fdaaa6d`), `feature/plan-021-s1b-asks` (#42, `db7686e`),
`feature/plan-021-s1b-state` (#46). All seven exit clauses met, each
against a named test; the criterion-by-criterion table, the execution
amendments X1–X59, the live smoke record, and V2/V3/V5/V6/V7 are in `12`; what
it found and did not do is the backlog in `13`. No
golden moved and no `testdata/` file changed except additions;
`craze prompt --json` is byte-identical apart from `seq` (V2: 103/103 SAME at
the tip). One defect was found live and fixed before the tip: quitting while a
turn ran left it `started` and never `ended`; `Close` now authors that turn's
ending itself before the session closes (`12`, "Deviations", item 11). The
permission path is still without live coverage on either platform — no agent
this smoke could drive ever asks one, on Linux or the mac-mini — recorded as
open, not failing, because nothing in S1b caused it.

**Exit result (S1c):** shipped 2026-09-24, plan
`024-session-control-s1c-transcript-model`, two sequential PRs from fresh
`origin/main` — `feature/plan-024-s1c-model` (#50, `27c1db6`),
`feature/plan-024-s1c-tui` (#52, merged 2026-09-24). All three exit clauses
met, each against a named test; the criterion-by-criterion table, the
execution amendments X1–X39, the live smoke record, and the measurements are
in `12`; what it found and did not do is the backlog in `13`. No golden
moved except four permitted one-row changes: SF-01's owner decision
(`native-echo-80x24` row 20, `native-mode-100x30`'s separator row, X38) and a
pre-existing macOS-CI flake fixed test-side (`grok-subagent-rows-{80x24,100x30}`'s
frozen spinner glyph, `✴` → `✳`, X39); `transcript_test.go`'s mixed
assertions moved packages under their own names with no behaviour change.
Live smoke ran on Linux (cursor, grok, native) and the mac-mini (grok,
native; cursor skipped there, the login keychain over ssh, as every earlier
phase found too), plus the hidden attach probe on every reachable provider,
SAME throughout (PR 1's Linux probe, PR 2's Linux and mac-mini probes). SQ9
is resolved (`10`); SQ15 stays open, measured but not decided. SF-01 and
SF-03 shipped here; SF-04 stays out; SF-02 is re-pointed at S2, which also
inherits `Engine.Attach`, `SubscribeOptions.Ctx`, the snapshot codec and its
version, and the receipt-mode gap (`12`, "Handoff").

### S2 — control socket

Detail in `05`. The protocol package, schema, socket server in the host,
`craze bridge`, a minimal `craze attach` (a second TUI on a running session,
which is also the best test client), the fake host, published reference docs.

- Size: M, about 3k lines.
- Owed by S1b and taken here (`13`, SF-10 to SF-19): a connection-local barrier
  so a reply is ordered after its events (`Control.Sync` returns no sequence
  number); a client id minted per connection, bound to it and released on
  disconnect; the gate table's remaining rows and the retry policy by code in
  the schema; index writes kept off connection goroutines; the TUI's engine
  calls moved onto `tea.Cmd`s; how a remote client words another client's
  settings change.
- Settled in S2 even though they pay off later (`02`, `05`): the runtime
  namespace (short, absolute, validated, lifetime locks, identity-checked
  unlink, peer checks both ways); transport close vs view close vs
  `session.stop`; connection-level vs session-level capabilities,
  subscription ids, the roster's own epoch; one attached session per
  connection (SQ14); bounded history pages; what `craze bridge` may assume
  under an SSH exec; what happens when `--continue` starts a second host for
  one session (SQ16). **SQ12 is decided (SD-33): hosts are born detached from
  S4 on and the TUI is a socket client for good**, so the socket-backed
  `Session` is the TUI's permanent path and S2's exit adds "the full TUI runs
  unchanged over it, goldens included".
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
the session (SD-33), the hub at `run/hub.sock` with roster, routing, spawn,
and stop; `craze ps`; `craze attach <id>`; `session.create` turns shed's
`create` capability on.

- **Detach is not a key binding (SD-33).** The TUI process is a job of the
  terminal's shell; Go cannot fork without exec and a process-group leader
  cannot `setsid`; closing a roost or tmux tab, or a systemd login scope with
  `KillUserProcesses=yes`, kills it whatever it ignores. The robust shape,
  decided as SD-33, is **hosts born detached, with the TUI always a socket
  client**; what a host does when its last client leaves (stop, or keep
  running) is then policy. Spawning from a bridge must fully detach
  (`setsid`, stdio to `/dev/null`) or the SSH channel never closes. A
  headless host parks asks with zero clients (`03`), and on macOS cannot
  start `cursor-agent` from a plain SSH exec (locked login keychain).
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
- A phase is not complete until everything it found and did not do is a row in
  `13`, and every `13` row it closed is deleted.
