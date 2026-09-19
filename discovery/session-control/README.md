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
| 2026-09-19 | **S1a executed and complete**: the event log, the lossless codec, the journal, and `seq` on `craze prompt --json`. Live smoke on Linux against cursor, grok and native; outcome, deviations, measurements and handoff in `12`. |

**Current phase: S1a complete. S1b (the engine turn driver, the ask registry,
and the shared/client-local split of TUI-authored rows) is next, under its own
panel-reviewed plan; progress is recorded in `12`.** S1–S5 are committed;
S6–S7 are directional and get decided after S1–S5 are in daily use (SD-12).

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

## How to use this folder in a future session

- Planning a phase: read `README`, `01`, `02`, `07`, `08`, then the topic
  file for the phase (`03`+`04` for S1, `05` for S2, `06` for S3).
- Any plan touching `internal/agent` or the native adapter: read `11` first.
- When a phase's plan is final, add its section to `12` as planned. When it
  merges, in the same PR: fill in `12`, update `07`'s phase table and this
  README's status table, and add an **Exit result** under the phase in `07`.
- Decisions are never edited. A reversal is a new `SD-nn` row naming the old
  one. Resolved questions stay in `10`, rewritten in place as
  `**Resolved (SD-nn).**`.
