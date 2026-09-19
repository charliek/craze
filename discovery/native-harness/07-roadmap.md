# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one or two
PRs, gated per commit, closed by a mac-mini live smoke. The provider stays
hidden throughout (`01`). Sizes are guidance from the reference reviews.

| ID | status | one line |
|---|---|---|
| H0 | complete | Fantasy `v0.43.2` providers fit all four provider classes; `Agent.Stream` fits behind a finish-normalizing wrapper; Catwalk is not embedded |
| H1 | in progress | skeleton: `CRAZE_HOME` (replaces `CRAZE_CONFIG`), two-file model/provider config imported once via `craze import gx`, provider factory, wrapper + `Agent.Stream` turn runner over the JSONL tree store, hidden native provider, effort option, cancel; no interject, no modes (D-34) |
| H2 | not started | tools: read, bash, edit, write, then ls, glob, grep; truncation wrapper; kind metadata; edit diffs; doom-loop guard; scripted-model test harness |
| H3 | not started | permissions: once / always / reject-with-feedback, prefix table, dangerous list, per-workspace grants, Claude rule syntax, yolo |
| H4 | not started | Claude compat: instruction files with imports and `paths` gating, workspace skills and commands, `craze import claude` for global instructions, user skills, and marketplace plugins |
| H5 | not started | modes: plan mode in the dispatcher, exit-plan and question tools, todos |
| H6 | not started | sub-agents: child-process agent tool, depth 1, derived permissions, personas from workspace and imported agents |
| H7 | not started | resume and compaction over the store, `--continue`/`--resume`/rename, cost in the status row |
| H8 | not started | images: clipboard read per OS, composer attachments, vision flag strip |

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
  populated once by `craze import gx` (D-28); Fantasy provider factory; the
  finish-normalizing `LanguageModel` wrapper with its fixtures (D-21, D-25);
  store with header/message/model_change/effort_change entries; turn runner
  on `Agent.Stream` behind the harness's own interface (text and thought
  only, no tools).
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

### H2 — tools

- PR 1: read, bash, edit, write with the truncation and kind wrappers, the
  edit ladder, per-file mutation queue, diffs through `textdiff`.
- PR 2: ls, glob, grep; doom-loop guard; length-stop rule; orphaned-tool
  closing on cancel.
- Testing pattern established: scripted `LanguageModel` and local streamed
  fixtures.
- **Interject via a `PrepareStep` drain at tool boundaries**, turning
  `Capabilities.Interject` on (D-34): with tool steps in place there is
  finally a safe point to merge steered text into history between steps.
- **Exit**: the agent makes a real change in the craze repo, on Linux and
  on the mac-mini, with tool cards and diffs rendering as they do for grok.

### H3 — permissions

- Permission wrapper, grant file, arity table, dangerous list, compound
  command splitting, Claude rule syntax parser, reject cascade with
  feedback, `Force` removes the wrapper.
- **Exit**: permission cards behave as for grok; "always" survives a
  restart; `Bash(git *)` written by hand in the grant file works.

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
