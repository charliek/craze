# Sub-agents — discovery

Durable findings and the roadmap for running **craze sessions as sub-agents**
of another agent, first Claude Code. An Opus orchestrator hands a task to
GPT-6.1 Sol on the ChatGPT plan, Muse Spark, cursor-agent or grok through
craze, with the feel of a native Claude Code sub-agent.

This folder is not user documentation and not a plan. Panel-reviewed plans
live outside the repo (`CLAUDE.md`); nothing here is published by `make docs`.
It is the sibling of `discovery/native-harness/` and
`discovery/session-control/` and follows their conventions, with its own ID
namespaces: phases `T1`–`T3`, decisions `TD-nn`, open questions `TQ-n`,
follow-ups `TF-nn`.

## Status

| date | event |
|---|---|
| 2026-10-10 | Discovery session. A Python prototype drove hub sessions as Claude Code sub-agents through a Haiku 5.5 wrapper; tested live on cursor, grok, native Muse and native ChatGPT Sol, including a `SendMessage` follow-up and an end-to-end plugin run from a headless Opus. A same-model review against `codex exec`. Owner's calls TD-01, TD-02, TD-04. Folder written. |
| 2026-10-10 | Plan 041 (T1) written and panel-reviewed (Codex `gpt-6-astra`, GLM 5.3, CodeRabbit): TD-08 (direct spawn, no protocol change) and TD-09 (no background work on unattended hosts); `03` carries a revision note. |
| 2026-10-11 | T1 done (plan 041): `craze wait`/`cancel`/`stop`, `craze new --wait`, `craze send`, unattended hosts, the `-c` skip and `sub-agent` tag, `craze prompt --effort/--fast`, the inline plugin. Live checks L1–L10 passed; TD-10–TD-16 record what they decided. |

**Current phase: T1 done (plan 041); T2 is directional.** T1's exit result is
in [07](07-roadmap.md); what it left is in [10](10-follow-ups.md).

## Reading order

1. [01 — Goals and scope](01-goals-and-scope.md): the idea, the owner's direction, non-goals.
2. [02 — Experiments](02-experiments.md): what ran on 2026-10-10 and what it showed, with numbers.
3. [03 — Design](03-design.md): the layers, the commands, the result, the host and protocol additions, `craze -c`.
4. [04 — Parity](04-parity.md): native Sonnet sub-agent against craze, before and after T1.
5. [05 — Flag gaps](05-cli-gaps.md): which session flags each command takes, and the gaps G1–G9.
6. [06 — Plugin](06-plugin.md): the inline Claude Code plugin, as shed and prox ship theirs.
7. [07 — Roadmap](07-roadmap.md): phases T1–T3 with exit criteria.
8. [08 — Decisions](08-decisions.md): append-only, `TD-nn`.
9. [09 — Open questions](09-open-questions.md): `TQ-n`, each with a default.
10. [10 — Follow-ups](10-follow-ups.md): the backlog, `TF-nn`.

## How to use this folder in a future session

- Planning a phase: read this README, `01`, `03`, `07`, `08`, `09`, and the
  phase's rows in `10`. The plan says which `TF-nn` rows it takes.
- Closing a phase: everything it found and did not do becomes a row in `10`,
  and every row it closed is deleted. Update this README's status table and
  add an exit result under the phase in `07`, in the same PR.
- Decisions are never edited. A reversal is a new `TD-nn` row naming the old
  one. Resolved questions stay in `09`, rewritten in place as
  `**Resolved (TD-nn).**`.
