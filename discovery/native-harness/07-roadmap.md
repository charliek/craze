# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one or two
PRs, gated per commit, closed by a mac-mini live smoke. The provider stays
hidden throughout (`01`). Sizes are guidance from the reference reviews.

| ID | status | one line |
|---|---|---|
| H0 | complete | Fantasy `v0.43.2` providers fit all four provider classes; `Agent.Stream` fits behind a finish-normalizing wrapper; Catwalk is not embedded |
| H1 | complete | skeleton shipped: hidden `native` provider; 14 of 15 imported models held a clean TUI turn, multi-turn sessions and `craze prompt --json` ran on a subset (OpenRouter via `openaicompat`, D-35); no interject, no modes (D-34) |
| H2 | in progress | tools: opencode's ported contract (`read`, `write`, `edit`, `bash`, `grep`, `glob`, D-38); the gate seam allowing everything (D-39); ripgrep on `PATH` (D-41); the doom-loop guard, nudge-then-stop (D-42); abnormal-finish handling (D-43); interject |
| H3 | not started | approval: owner's direction is an auto-mode evaluator over the H2 gate, not ask-on-everything; scope decided when planned, after session-control S1 (D-39; Plan 019 §3.3) |
| H4 | not started | Claude compat: instruction files with imports and `paths` gating, workspace skills and commands, `craze import claude` for global instructions, user skills, and marketplace plugins |
| H5 | not started | modes: plan mode in the dispatcher, exit-plan and question tools, todos |
| H6 | not started | sub-agents: child-process agent tool, depth 1, derived permissions, personas from workspace and imported agents |
| H7 | not started | resume and compaction over the store, `--continue`/`--resume`/rename, cost in the status row |
| H8 | not started | images: clipboard read per OS, composer attachments, vision flag strip |
| HL | unscheduled | own the turn loop — see D-40's triggers |

## Phase detail

### H0 — spike

Completed on 2026-09-18. The external Go 1.27 probe pinned released Fantasy
`v0.43.2` and Catwalk `v0.52.43`; no unused root dependency was added.

- The fixed 11-model Linux campaign ran through both a bounded public
  `LanguageModel.Stream` driver and released `Agent.Stream`. Eight aliases
  passed `S,S` in both modes, DeepSeek V4 Pro was rejected by Fireworks with
  HTTP 404, and both Meta aliases exhausted their attempt caps (see the
  review below for why).
- A deterministic local fixture proved that the direct driver continues a
  complete tool call reported with finish reason `stop`, while released
  `Agent.Stream` ends after one request and does not dispatch it.
- Catwalk required two demonstrated reasoning-contract corrections: Kimi K2.7
  Code's false capability flag and MiniMax M3's unsupported effort
  levels/default. It therefore crossed the no-go threshold.
- gx reachability controls were not dispatched because gx exposes no
  enforceable numeric output-token ceiling. Effort/replay and cancellation
  variants ran; image variants were skipped because the approved live
  manifest is text-only.
- Four macOS provider representatives used the identical approved probe
  manifest and only credentials already present on the mac-mini. Detailed
  attempt sequences and platform limits are recorded in `11`.

A review on 2026-09-18 re-ran what H0 could not explain (`04`, `11`):

- The Meta failures were the probe's 512-token ceiling truncating reasoning,
  which Meta reports as an empty `stop`. Both aliases then passed 13 of 13
  released `Agent.Stream` loops.
- The `stop`-with-tool-call hazard never appeared live, and a roughly 40-line
  `LanguageModel` wrapper removes it.
- DeepSeek V4 Pro's 404 was a stale wire id in gx's configuration.

**Exit result:** Fantasy's provider layer fits; `Agent.Stream` fits behind the
wrapper (D-21); the catalog is a craze-owned model table (D-22); the Go floor
is 1.27.0 with toolchain 1.27.1, landed separately (D-23); ten of 11 aliases
are qualified (D-24). Nothing blocks H1.

### H1 — skeleton

- `internal/harness`: `CRAZE_HOME` (single env var, replaces
  `CRAZE_CONFIG`, D-27); two-file config, `providers.toml` + `models.toml`,
  populated once by `craze import gx` (D-28); Fantasy provider factory
  (OpenRouter via `openaicompat`, not `providers/openrouter`, D-35); the
  finish-normalizing `LanguageModel` wrapper with its fixtures, including the
  `refusal` stop reason (D-21, D-25, D-36); store with
  header/message/model_change/effort_change entries; turn runner on
  `Agent.Stream` behind the harness's own interface (text and thought only,
  no tools).
- `internal/agent`: `NativeProvider()` with `hidden: true`; session adapter;
  `Capabilities{Effort}` only — interject stays off until H2's `PrepareStep`
  drain, modes stay off until H5's dispatcher (D-34); model dialog lists the
  model table; effort as a config option.
- A depguard rule fails any import of `internal/agent`, `internal/tui`,
  `internal/acp`, `internal/cli`, `internal/sessions`, `internal/host`, or
  `internal/paths` from `internal/harness` (D-02, D-26; Plan 018 §3.1).
- Output ceilings come from the model table and leave room for reasoning
  (D-25).
- **Exit**: a conversation with all ten qualified models from the real TUI
  via `--provider native`, mid-session model switch, cancel, and
  `craze prompt --json` unchanged (no interject in H1). DeepSeek V4 Pro
  surfaces `ErrModelNotFound` until its wire id is corrected, then joins
  once it passes one tool loop. Picker never shows the provider.

**Exit result:** shipped across PRs #28, #30, and #31. `craze --provider
native` (hidden) held one clean TUI turn on 14 of 15 imported models, and
multi-turn sessions (switching across OpenRouter, Fireworks and Meta) and
`craze prompt --json` ran on a subset of them; `fireworks/deepseek-v4-pro`
surfaces its stale gx wire id as a 404 (`ErrModelNotFound`-shaped), exactly as
D-24 expected. Mid-session model switch, effort switch, cancel, and queued
follow-ups all round-tripped live. A bug surfaced in the first live session —
after switching from a model with no efforts to one with efforts, the TUI
never re-read the snapshot, hiding the new effort row and failing
`/model <id> <effort>` — fixed in b2c851c (the adapter now emits a bare
`EventMeta` after `SetModel`/`SetConfig`) and re-verified clean in a second
session. Two decisions were made during execution: OpenRouter runs on
`openaicompat`, not `providers/openrouter` (D-35, since confirmed by the
owner), and a content-filter finish maps to `refusal` (D-36).

Live smoke, Linux (2026-09-19), one TUI turn per imported alias:

| alias | result |
|---|---|
| `fireworks/kimi-k3` | clean, thinking shown |
| `fireworks/qwen3p8-max` | clean, thinking shown |
| `fireworks/kimi-k2p7-code` | clean, thinking shown |
| `fireworks/deepseek-v4-flash` | clean, thinking shown |
| `fireworks/deepseek-v4-pro` | 404, stale wire id — expected (D-24) |
| `glm-5.3` | clean, no reasoning text streamed |
| `glm-5.3-flash` | clean, thinking shown |
| `muse-spark-1.3` | clean, no reasoning text streamed |
| `muse-spark-1.3-contributor` | clean, no reasoning text streamed |
| `openrouter/minimax-m3` | clean, thinking shown, no effort row |
| `openrouter/gemini-3.8-flash` | clean, no reasoning text streamed |
| `openrouter/glm-5.3-flash` | clean, no reasoning text streamed |
| `openrouter/gpt-5.6-luna` | clean, no reasoning text streamed |
| `openrouter/gpt-5.6-terra` | clean, no reasoning text streamed |
| `openrouter/gpt-5.6-sol` | clean, no reasoning text streamed |

macOS (mac-mini, cross-compiled darwin/arm64): `craze import gx` produced the
same 4 providers and 15 models; one TUI turn each on `fireworks/kimi-k3`
(thinking shown), `glm-5.3`, and `muse-spark-1.3` all answered clean.

No disruption: on Linux, `config.toml` (`provider = "cursor"`) and
`sessions.jsonl` were byte-identical (size, mtime, SHA-256, 15 rows) after 16
native sessions and the import; a plain `craze` still opens the picker with
cursor as default and no native row; `--continue` still restored an ACP
(grok) session. On the mac-mini, `config.toml` (`provider = "grok"`) was
unchanged and no `sessions.jsonl` was created.

Cache-read tokens were observed (recorded, not a pass condition): OpenRouter
`minimax-m3` 148 on a session's first turn (prefix shared with earlier runs);
Meta `muse-spark-1.3` 241 and 497 on later turns; Fireworks `kimi-k2p7-code`
219 on the third turn; 0 on the first turn after each model switch.

Binary size: stripped `bin/craze` grew 9.5 MB → 32.4 MB once the harness
linked (C9) — not an SDK (the deps check covers `./cmd/craze`). The cause was
one line in craze, not the harness: `SetVersionTemplate` on the root command.
cobra 1.10 reaches `text/template`'s executor only through its `Set*Template`
calls; that executor looks methods up by name, so the linker kept every
exported method of every linked type, which cost 1.7 MB before the harness and
13.7 MB with openai-go linked (34,541 of its symbols against 9,495). craze now
prints `--version` itself and a test links the binary to keep run-time method
lookup out (D-37): 18.7 MB stripped, against 7.8 MB for the same fix on the
pre-harness tree. D-26's binary-size trigger is not tripped.

Known limitations / follow-ups: DeepSeek V4 Pro's wire id needs correcting
(in gx before re-importing, or in `models.toml` with `source = "manual"`);
the default model after import is the lexicographically first alias when
gx's own default was skipped (`fireworks/deepseek-v4-flash` on Linux); the
harness's one-line error cleanup drops an ESC byte before the adapter's
sanitizer can strip the whole escape sequence, so a provider message can show
harmless cosmetic leftovers like `[2J`; interject (H2), modes (H5), idle
timeout (H2), resume/indexing (H7), and cost display (H7) remain as planned.

### H2 — tools

Plan 019, four sequential PRs, each branched from `main` after the previous
one merges (Plan 019 §5). No idle timeout: H1's follow-up list above names
"idle timeout (H2)", but nothing specifies it for H2; it moves to HL/H7
(Plan 019 §4).

- **PR 1 — foundations (C1–C3, no user-visible change)**: this docs commit
  (C1); the store's `AppendStep`, pairing invariant, and tail rollback for
  tool steps (C2); `internal/harness/redact` and the `tool` framework —
  `Tool`/`Prepared`/`Spec`/`Result`/`Env`, the dispatcher, the gate, the
  profile registry, truncation, the per-path lock, description rendering —
  tested with fake tools only (C3).
- **PR 2 — the tools, not yet wired (C4–C7, no user-visible change)**:
  `read`/`write` and the `opencode` profile (C4); `edit` (C5, reviewed
  alone, adversarial); `bash` (C6, reviewed alone, process lifetime and
  concurrency); `grep`/`glob` (C7, CI gains ripgrep).
- **PR 3 — switching tools on (C8–C11)**: the runner wired to
  `toolbridge.go` with per-turn state, tool events and ids, cancel
  precedence, abnormal-finish handling (D-43), `max_turn_requests`, and the
  new system prompt (C8, reviewed alone, re-runs D-40's constraint table in
  its commit message); the doom-loop guard (C9, D-42); the adapter's tool
  event merge and TUI golden frame (C10); the pytest tool-loop fixtures
  (C11). This is where tools first reach users.
- **PR 4 — interject, smoke, record (C12–C14)**: `Steer`/`PrepareStep`
  drain turning `Capabilities.Interject` on (D-34) — with tool steps in
  place there is finally a safe point to merge steered text into history
  between steps (C12, reviewed alone, races); the live smoke and any fix it
  finds (C13); this docs update, recording the smoke tables and D-40's
  second table re-run (C14).
- **OpenRouter tool-loop check (D-35)**: run a multi-step tool loop on the
  OpenRouter reasoning models. If one degrades or fails without
  `reasoning_details` replay, first carry the replay in craze's own wrapper;
  the owner accepts `providers/openrouter` and its SDKs if quality needs it.
- **Exit**: the agent makes a real change in the craze repo, on Linux and
  on the mac-mini, with tool cards and diffs rendering as they do for grok.

### H3 — approval

Reworded from "permissions" (Plan 019 §3.3): D-12 and `05`'s permission
model stay as written but lose their phase. The owner's direction is
**likely an auto-mode evaluator layered over H2's gate** (`Gate.Check`),
not ask-on-everything — a model or evaluation step flags only
dangerous-looking actions, perhaps through hooks — but the scope is decided
when this phase is actually planned, **after session-control S1** lands its
engine-owned ask registry (D-39; `10` Q6). Nothing in H2 reads or writes a
grants file.

- **Exit**: not yet defined; depends on the shape chosen when planned.

### H4 — Claude compat

- Instruction loader with imports, rules with `paths`, lazy injection;
  skills catalog in the prompt; commands; `craze import claude`; compat
  toggles.
- **Exit**: this repo's `CLAUDE.md` is followed; a workspace skill and an
  imported plugin skill both appear in the slash menu and expand; the
  import command reports added / upgraded / kept.

### H5 — modes

- Plan, ask, implement as rulesets + reminders; dispatcher enforcement;
  `exit_plan_mode` and `ask_user_question` tools; `todo_write`.
- **Exit**: a plan-mode round trip with the plan card and accept →
  implement; a question card answered; todos in the tasks panel.

### H6 — sub-agents

- Child-process `agent` tool, event tagging, cancel propagation, personas.
- **Exit**: a task fanned out to two children with both transcripts in the
  sub-agent view.

### H7 — resume and compaction

- Replay, `--continue`, `--resume`, rename over the store; compaction with
  the fixed template, 20k-token tail, segment files; cost in the status
  row.
- **Exit**: resume a compacted session; spend visible per turn.

### H8 — images

- Clipboard image read (Linux, macOS), composer attachment, `FilePart`,
  per-model strip with placeholder.
- **Exit**: paste a screenshot, GLM gets the placeholder, a vision model
  gets the image.

### HL — own the turn loop (unscheduled)

Not a numbered phase: no plan exists yet. Recorded so H2's pragmatic choice
to stay on Fantasy's `Agent.Stream` (D-40) is not forgotten. Any one of
these five triggers starts a plan:

- H7 needs persist-before-run or mid-turn compaction.
- A Fantasy bug in the loop cannot be worked around from outside.
- A Fantasy upgrade changes callback semantics the runner depends on.
- The gate needs to pause a whole step rather than one call.
- The workaround table (`08-decisions.md`, "D-40 constraint table") gains a
  row that costs more than a day.

Estimated 500–700 lines plus tests, replacing `turn.go`'s driver and
`toolbridge.go` with a craze-owned `LanguageModel.Stream` loop (H1's
wrapper, Fantasy's providers, and `jsonrepair` all still apply). D-40's
constraint table is re-run on evidence in C8's commit message and after H2's
live smoke (C14); see `08-decisions.md`.

## Deferred (not scheduled)

settings.json translation · hooks · MCP and plugin `.mcp.json` · native
marketplace installer · ACP server binary · ChatGPT-plan (codex) auth ·
background bash with auto-background · in-process sub-agents · sandboxing ·
flipping the provider to visible.

## Conventions per phase

- Plan first (panel review), then the repository's normal gated implementation
  flow.
- `make lint && make test && make test-race && make build && make test-cli`
  per commit; `make docs` when published docs or docs tooling changes.
- Update `08-decisions.md` when a phase changes a decision; update the
  status column above when a phase merges.
- The harness is a side quest beside the daily-driver TUI (D-26): size each
  phase's process to its risk. A spike is a throwaway `main`, and it keeps
  raw provider responses (credentials stripped) so a failure can be diagnosed
  rather than retried. H0's recorder discarded them, which is why its Meta
  failures were mislabelled.
