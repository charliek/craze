# Native web search — discovery

Durable findings and a proposed order of work for **native web search after its
first release**: the `websearch` and `webfetch` tools that plan 051 shipped in
v0.3.0, reached through Exa's hosted MCP endpoint
(`internal/harness/websearch`, `docs/reference/configuration.md` § Native web
search).

This folder is not user documentation and not a plan. Panel-reviewed plans
live outside the repo (`CLAUDE.md`), and nothing here is published by
`make docs`. It is a sibling of `discovery/native-harness/`,
`discovery/session-control/` and `discovery/subagents/` and follows their
conventions, with its own ID namespaces: phases `W1`–`W3`, decisions `WD-nn`,
open questions `WQ-n`, follow-ups `WF-nn`.

## Status

| date | event |
|---|---|
| 2026-11-15 | Discovery. In a real research session (a Wren design question), web search lost 5 of 22 searches to Exa's keyless rate limit, and a key stored mid-session was not used. The endpoint was then probed directly with the stored key ([02](02-experiments.md)). Issues #137–#141 were filed with measurements, #131 was annotated, and this folder was written. Nothing is decided yet. |

**Current phase: none started.** [04](04-roadmap.md) proposes W1–W3; the owner
has not reviewed them.

## Reading order

1. [01 — Goals and scope](01-goals-and-scope.md): what went wrong in use, what "good" means, non-goals.
2. [02 — Experiments](02-experiments.md): E1–E9, with commands and numbers.
3. [03 — Design](03-design.md): the proposed change in each area, with code pointers.
4. [04 — Roadmap](04-roadmap.md): W1–W3 with exit criteria and the issue each takes.
5. [05 — Decisions](05-decisions.md): append-only, `WD-nn` (none yet).
6. [06 — Open questions](06-open-questions.md): `WQ-n`, each with a default.
7. [07 — Follow-ups](07-follow-ups.md): the backlog, `WF-nn`, mapped to GitHub issues, in priority order.

`probe/exa_probe.py` is the standalone probe behind [02](02-experiments.md). It
uses the Python standard library only, reads the stored key from
`credentials.toml`, and never prints it.

## How to use this folder in a future session

- Planning a phase: read this README, `01`, `03`, `04`, `06`, and the phase's
  rows in `07`. The plan says which `WF-nn` rows (and issues) it takes.
- Closing a phase: delete every row it closed from `07` and close the issue,
  record anything found and not done as a new row, add an exit result under
  the phase in `04`, and update the status table above, in the same PR.
- Decisions are never edited. A reversal is a new `WD-nn` row naming the old
  one. A resolved question stays in `06`, rewritten in place as
  `**Resolved (WD-nn).**`.
- The eval cannot see most of what is here: `eval/` answers web calls from a
  local corpus proxy (`CRAZE_TEST_EXA_MCP`), so rate limits, key handling and
  Exa's error texts only show against the real endpoint. Each phase needs a
  live check ([04](04-roadmap.md)).
