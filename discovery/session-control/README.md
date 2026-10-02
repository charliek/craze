# Session control — discovery

Durable findings and the roadmap for letting **more than one client attach to
a craze session**: a documented local control socket (first consumer:
shed-mobile and shed desktop), headless sessions with an in-TUI agent view,
and, directionally, a web UI and a remote relay.

This folder is not user documentation and not a plan. Panel-reviewed plans
live outside the repo (`CLAUDE.md`); nothing here is published by `make docs`.
It is the sibling of `discovery/native-harness/` and follows its conventions,
with its own ID namespaces so the two never collide: phases `S0`–`S7`,
decisions `SD-nn`, open questions `SQ-n`.

## Status

| date | event |
|---|---|
| 2026-09-19 | Discovery session: craze, prox, shed + shed-mobile + gx, and t3code reviewed; owner settled topology, protocol, remote scope, and journaling (SD-02, SD-03, SD-07, SD-12). Folder written. |
| 2026-09-19 | craze-harness session briefed on the overlap (`11`). |
| 2026-09-19 | Panel review (Codex `gpt-6-astra`, CodeRabbit, GLM 5.3): SD-18 to SD-31, SQ12–SQ16, S1 re-sized; record in `12`. |
| 2026-09-19 | Plan 020 (S1a) written and panel-reviewed; SD-32. Not yet executed. |
| 2026-09-19 | **S1a executed and complete**: the event log, the lossless codec, the journal, and `seq` on `craze prompt --json`. Live smoke on Linux (cursor, grok, native) and on the mac-mini (grok and native; `cursor-agent` skipped there, blocked by the login keychain over ssh). A native tool call could not be exercised on either platform until H2 gives the harness tools. Outcome, deviations, measurements and handoff in `12`. |
| 2026-09-20 | Plan 021 (S1b) written and panel-reviewed; SD-33 (SQ12: born detached). Execution started. |
| 2026-09-21 | **S1b executed and shipped**: the engine turn driver, the queue and send-now, the ask registry, settings as state deltas, command ids, and the durable craze session id. Three PRs (#41 `fdaaa6d`, #42 `db7686e`, #46). Live smoke on Linux (cursor, grok, native) and the mac-mini (grok, native; cursor skipped there, blocked by the login keychain over ssh, as S1a found too). Outcome, deviations, smoke tables and handoff in `12`. |
| 2026-09-24 | **S1c executed and shipped**: the render-free `internal/transcript` model, folded inside the engine's boundary and by every client; bounded lossless snapshots; in-process attach (`Engine.Attach` on `Control`); the TUI became its first client with an explicit display list. Two PRs (`feature/plan-024-s1c-model` #50 `27c1db6`, `feature/plan-024-s1c-tui` #52, merged 2026-09-24). Live smoke on Linux (cursor, grok, native) and the mac-mini (grok, native; cursor skipped there, the login keychain over ssh again), plus a hidden attach probe on every reachable provider, SAME throughout. **S1 is now complete.** Outcome, the execution amendments, smoke tables and handoff in `12`. |
| 2026-09-27/28 | **S2 executed and shipped**: the protocol package and published schema, the control socket server, `internal/remote`, the runtime namespace, `craze bridge`, the TUI's engine calls moved onto `tea.Cmd`s behind a command gate, and `craze attach` — the full TUI over a running session's socket, restores and resume included. Four PRs (`feature/plan-027-s2-wire` #55 `318fc76`, `feature/plan-027-s2-host` #56 `2acd54a`, `feature/plan-027-s2-tui-async` #61 `3eabb31`, `feature/plan-027-s2-attach` #63 `73ed5e0`). Live smoke on Linux (cursor, grok) and the mac-mini (grok, native; cursor skipped there, the login keychain over ssh again), plus every frame golden run over the socket as well as in process. **S2 is now complete.** Outcome, the execution amendments X1–X58, smoke tables and handoff in `12`; the backlog it leaves is `13`. |
| 2026-09-28 | Plan 030 (S4a + S5) written and panel-reviewed in two rounds. The owner reordered the roadmap: S4a (detached hosts) and S5 (the agent view) next, then S4b (the hub), then S3 after shed has made its first release of its own lane work; SD-34, SD-35. Roadmap docs refactored (`07`, `08`, `10`, `11`, `12`, `13`) as the plan's first PR (PR 0 of five); execution continues with PR 1. |
| 2026-09-28/30 | **S4a + S5 executed and shipped**: `craze serve`, every session in a detached host that outlives its terminal, `session.stop` and `/exit` ending a session in every client, the idle exit; the session list (`←`, `/sessions`) of every running session on the machine, opening one in place, saved sessions resumed from it; new sessions started from the list's input with an `@` directory picker, `/provider` and `/model`, and a model catalog cache; composer `@` file and folder mentions. Five PRs (`docs/plan-030-roadmap` #66 `77f1cd3`, `feature/plan-030-hosts` #68 `6be2273`, `feature/plan-030-sessions-list` #70 `b8aa5bb`, `feature/plan-030-new-sessions` #71 `b401fec`, `feature/plan-030-composer-at`). Live smoke on Linux (cursor, grok, native; tmux and cosmic-term) and the mac-mini (grok, native; an ssh logout and login; cursor skipped there, the login keychain over ssh again). **S4a and S5 are now complete.** Outcome, the execution amendments X1–X202, smoke tables and handoff in `12`; the backlog it leaves is `13`. |
| 2026-09-30 | Plan 032 (S4b, the hub) written and panel-reviewed in three rounds. The owner's calls: SD-36 (every journal kept), SD-37 (the hub auto-spawned on demand, exiting when idle, learning hosts from the registry), SD-38 (S4b local-only; listing other machines becomes S4c), SD-39 (the provider's replay stays the resume authority); `02`, `05`, `07`, `10` and `13` amended in its first commit. |
| 2026-10-01/02 | **S4b executed and shipped**: the per-machine hub (`craze hub`, auto-spawned, exiting when idle), its roster (`sessions.list`, `sessions.subscribe`), the `session.connect` splice and `session.create`; `craze ps`, `craze new` and `craze bridge --hub`; the session list on the hub's subscription, with its poller as the fallback; `--effort`/`--fast` at a session's start and per-provider agent binaries (`[agents]`); presence (`N attached`); with it the owner's rows (SF-86, SF-99, SF-61), the transcript's render cost (SF-102) and the agent reaper (SF-80). Five PRs (`feature/plan-032-rows` #75 `2eec72f`, `feature/plan-032-render` #76 `2dc80b7`, `feature/plan-032-hub` #78 `bc8da8f`, `feature/plan-032-create` #79 `8a33a12`, `feature/plan-032-reaper` #81). Live smoke on Linux (cursor, grok, native; a real `a0d88c3` host behind the hub) and the mac-mini (grok, native and, for the first time there, cursor, from the GUI login session's tmux server; a hub first started over ssh cannot start cursor, SF-126). **S4b is now complete.** Outcome, the execution amendments X1–X75, smoke tables and handoff in `12`; the backlog it leaves is `13`. |

**Current phase: S4b is complete; S3 (the shed lane) is next, after shed's
first release of its own lane work.** S1, S2, S4 (S4a and S4b, SD-34) and S5
are done; S3 is committed and next; S4c, S6 and S7 are directional and get
decided after S1–S5 are in daily use (SD-12, SD-38).

## Reading order

1. [01 — Goals and scope](01-goals-and-scope.md): the three features, why they are one engine, non-goals.
2. [02 — Architecture](02-architecture.md): session hosts, the hub, clients, the remote direction, security posture.
3. [03 — Engine core](03-engine-core.md): fan-out, engine-owned asks and turn state, the transcript model, commands.
4. [04 — Journal](04-journal.md): the on-disk update log, and what makes it rich enough to mine.
5. [05 — Protocol](05-protocol.md): messages, attach and resume, transports, `craze bridge`, versioning, the executable spec.
6. [06 — shed lane](06-shed-lane.md): how the protocol maps onto shed's `AgentLane`, and what to do better than gx.
7. [07 — Roadmap](07-roadmap.md): phases S0–S7 with exit criteria and sizes.
8. [08 — Decisions](08-decisions.md): append-only log, `SD-nn`.
9. [09 — References](09-references.md): what to read in prox, shed, gx, roost, t3code, and craze itself.
10. [10 — Open questions](10-open-questions.md): `SQ-n`, each with a default.
11. [11 — Harness coordination](11-harness-coordination.md): overlap with `discovery/native-harness/` and the ordering that matters.
12. [12 — Progress and results](12-progress.md): what each phase actually did, with a template for recording one.
13. [13 — Follow-ups](13-follow-ups.md): the backlog, `SF-nn` — everything a phase found and did not do, by the phase that should take it.

## How to use this folder in a future session

- Planning a phase: read `README`, `01`, `02`, `07`, `08`, then the topic
  file for the phase (`03`+`04` for S1, `05` for S2, `06` for S3), **and the
  phase's section of `13`**: the plan's work breakdown says which `SF-nn` rows
  it takes and which it leaves.
- Closing a phase: before it is called complete, everything it found and did
  not do is a row in `13`, and every row it closed is deleted from it.
- Any plan touching `internal/agent` or the native adapter: read `11` first.
- When a phase's plan is final, add its section to `12` as planned. When it
  merges, in the same PR: fill in `12`, update `07`'s phase table and this
  README's status table, and add an **Exit result** under the phase in `07`.
- Decisions are never edited. A reversal is a new `SD-nn` row naming the old
  one. Resolved questions stay in `10`, rewritten in place as
  `**Resolved (SD-nn).**`.
