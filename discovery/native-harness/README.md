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
| 2026-09-17 | H0 started: exact candidate pins, the standalone probe, safety controls, and bounded live campaign were approved |
| 2026-09-18 | H0 complete, then reviewed: Fantasy `v0.43.2` providers fit all four provider classes, `Agent.Stream` fits behind a small wrapper, the catalog is a craze-owned table, Meta works ([progress and results](11-h0-progress-and-results.md)) |
| 2026-09-18 | Plan 018 (H1 walking skeleton) planned and started; decisions D-27..D-34 |
| 2026-09-19 | H1 complete: `craze --provider native` (hidden) held a clean turn with 14 of 15 imported models and multi-turn sessions across providers; live smoke on Linux and the mac-mini |
| 2026-09-19 | Plan 019 (H2 tools) planned and panel-reviewed; execution started; decisions D-38..D-43 |
| 2026-09-19 | H2 complete: opencode-ported tools (`read`, `write`, `edit`, `bash`, `grep`, `glob`) shipped across PRs #33, #34, #35, #37; live smoke on Linux (14 of 15 imported models) and the mac-mini (4 of 4) closed the phase; D-35's condition met, OpenRouter stays on `openaicompat` (D-44) |
| 2026-09-20 | Plan 022 (H4) planned and panel-reviewed; H4 re-scoped to live reading behind a seam (D-45..D-48) |
| 2026-09-20 | H4 compat work shipped across two PRs: #39 (content sources, discovery, expansion) and PR 2 (`feature/plan-022-h4-prompt`: prompt extras, instructions with imports and confinement, the model-facing catalog, `[compat.claude]` toggles); PR 3 (the composer shell mode) is outstanding, so H4 stays in progress |
| 2026-09-20/21 | H4 complete: PR 3 (#43, `be05da9`) landed the composer shell mode, its process runner, and shell output carried to the agent with the next prompt; live smoke round-tripped on cursor and on native (`fireworks/kimi-k3`), with grok covered by `TestShellContextNeverReachesTheScreen` rather than driven live; H5 is next |
| 2026-09-21 | Plan 023 (H5) planned and panel-reviewed: modes, the plan and question tools, todos; decisions D-49..D-53 |
| 2026-09-21/22 | H5 complete: PR 1 (#45, `b0ea4c4`) landed the harness side — the mode gate, the plan file, reminders, and the three tools (`ask_user_question`, `exit_plan_mode`, `todo_write`); PR 2 (#48, `feature/plan-023-h5-modes`) switched modes on in the adapter, the CLI, and the TUI, with the plan-offer eligible for a turn with no assistant text; live smoke: the first real `exit_plan_mode` on a live model behaved as designed |
| 2026-09-24 | Plan 026 (H6) planned and panel-reviewed: sub-agents run in-process (D-54, superseding D-10), the `agent` tool (kind `task`), personas, per-child cancel, and background children across three PRs; decisions D-54..D-59; not yet executed |
| 2026-09-24 | H6 foreground complete — PR 1 (#51) shipped in-process sub-agents end to end; PR 2 (`feature/plan-026-h6-stop`) adds per-child stop: Delete/Backspace on a running native row or in its view, `Control.CancelSubagent`; PR 3 (background children) next |
| 2026-09-25 | H6 complete — PR 3 (`feature/plan-026-h6-background`) ships background children: `run_in_background`, `agent_output`, the session-level wake bracketed as a foreign turn (`ForeignTurnInfo.Reason`, `agent.AdmissionFence`), the `bg` row marker, and SF-21 closed (a drained row refused by the wake is restored) |

**H0, H1, H2, H4, H5 and H6 are complete (Plan 026: PR 1 #51, PR 2 #53, and
PR 3's background children) — H3 (approval) still comes after H8 (D-48).**
[11-h0-progress-and-results.md](11-h0-progress-and-results.md) holds H0's
evidence and, at its top, the review that corrected four of its conclusions.
The harness is built in this repository as a hidden side quest beside the
daily-driver TUI (D-26).

## Reading order

1. [01-goals-and-scope.md](01-goals-and-scope.md) — what we are building, what we are not, who sees it
2. [02-architecture.md](02-architecture.md) — package split, the craze seam, the turn loop, the home directory
3. [03-session-store.md](03-session-store.md) — the JSONL tree that every other subsystem reads
4. [04-providers-and-catalog.md](04-providers-and-catalog.md) — Fantasy provider findings, the catalog decision, the one-time gx import, and per-provider behavior
5. [05-tools-and-permissions.md](05-tools-and-permissions.md) — the tool set, its limits, the permission model, plan mode
6. [06-claude-compat.md](06-claude-compat.md) — CLAUDE.md, skills, marketplace plugins: live vs imported
7. [07-roadmap.md](07-roadmap.md) — phases H0–H8 with exit criteria, plus the deferred list
8. [08-decisions.md](08-decisions.md) — the decision log; do not reopen a decision without a new entry
9. [09-references.md](09-references.md) — the clones under `../thirdparty/` and what to read in each
10. [10-open-questions.md](10-open-questions.md) — items still the owner's call
11. [11-h0-progress-and-results.md](11-h0-progress-and-results.md) — checkpoint chronology, sanitized H0 evidence, verdicts, and the exact resume point

## How to use this folder in a future session

- Read `README.md`, `01`, `02`, `07`, and `08` first. Read the rest as the phase needs them.
- Each roadmap phase becomes one panel-reviewed plan outside the repo and
  one or two PRs, gated by `make lint && make test && make test-race && make build && make test-cli` per commit
  (see `CLAUDE.md`). Live smoke on the mac-mini closes each phase.
- When a phase changes a decision, add a row to `08-decisions.md` and update
  the affected document. When it finishes, update the status table above and
  the phase table in `07-roadmap.md`.
- Sizes quoted in these documents are from reading the reference codebases
  and are guidance, not commitments.
