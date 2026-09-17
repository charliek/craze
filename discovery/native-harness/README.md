# discovery/native-harness/ — the native harness

This folder (`discovery/native-harness/`) is the durable home for the **native harness** work: a Go agent
harness built on [charm.land/fantasy](https://github.com/charmbracelet/fantasy)
that runs inside craze as a fourth provider, next to cursor, grok, and gx.
It exists so a future session can pick the work up without re-deriving it.

It is **not** user documentation. Nothing here is published by `make docs`
(that reads `docs/` only), and nothing here is a panel-reviewed plan
(those live outside the repo, per `CLAUDE.md`). Plans are written *from*
these documents, one per roadmap phase.

## Status

| date | event |
|---|---|
| 2026-09-15 | Discovery: fantasy, catwalk, acp-go-sdk inspected; opencode, grok-build, crush reviewed |
| 2026-09-16 | pi reviewed; scope narrowed (Claude compat via import, hooks/settings/MCP deferred); this folder written |

No code exists yet. The first implementation phase is **H0** in
[07-roadmap.md](07-roadmap.md).

## Reading order

1. [01-goals-and-scope.md](01-goals-and-scope.md) — what we are building, what we are not, who sees it
2. [02-architecture.md](02-architecture.md) — package split, the craze seam, the turn loop, the home directory
3. [03-session-store.md](03-session-store.md) — the JSONL tree that every other subsystem reads
4. [04-providers-and-catalog.md](04-providers-and-catalog.md) — fantasy, catwalk, the gx overlay, per-provider quirks
5. [05-tools-and-permissions.md](05-tools-and-permissions.md) — the tool set, its limits, the permission model, plan mode
6. [06-claude-compat.md](06-claude-compat.md) — CLAUDE.md, skills, marketplace plugins: live vs imported
7. [07-roadmap.md](07-roadmap.md) — phases H0–H8 with exit criteria, plus the deferred list
8. [08-decisions.md](08-decisions.md) — the decision log; do not reopen a decision without a new entry
9. [09-references.md](09-references.md) — the clones under `../thirdparty/` and what to read in each
10. [10-open-questions.md](10-open-questions.md) — items still the owner's call

## How to use this folder in a future session

- Read `README.md`, `01`, `02`, `07`, and `08` first. Read the rest as the phase needs them.
- Each roadmap phase becomes one panel-reviewed plan outside the repo and
  one or two PRs, gated by `make lint && make test && make build` per commit
  (see `CLAUDE.md`). Live smoke on the mac-mini closes each phase.
- When a phase changes a decision, add a row to `08-decisions.md` and update
  the affected document. When it finishes, update the status table above and
  the phase table in `07-roadmap.md`.
- Sizes quoted in these documents are from reading the reference codebases
  and are guidance, not commitments.
