# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one or two
PRs, gated per commit, closed by a mac-mini live smoke. The provider stays
hidden throughout (`01`). Sizes are guidance from the reference reviews.

| ID | status | one line |
|---|---|---|
| H0 | complete | Fantasy `v0.43.2` is a provider-only fit; released `Agent.Stream` and Catwalk `v0.52.43` are no-go |
| catalog replacement spike | required before H1 | select and verify the catalog source after Catwalk exceeded the correctness-correction threshold |
| H1 | blocked | skeleton using Fantasy `LanguageModel.Stream` plus a craze-owned step loop; waits for the catalog replacement decision |
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
  HTTP 404, and both Meta aliases exhausted their caps through HTTP-200 model
  noncompliance.
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

**Exit result:** Fantasy provider layer fit; Fantasy released agent loop
no-go; combined Fantasy verdict provider-only fit; Catwalk no-go. The root Go
floor is 1.27.0 with toolchain 1.27.1. H1 must own the direct stream loop and
wait for the catalog replacement spike.

### Catalog replacement spike — prerequisite

- Compare candidate catalog sources against all 11 fixed H0 alias records,
  including reasoning capability/defaults, attachments, provider
  identity/type/endpoint, context/output metadata, independently verified
  prices, and H0 support status.
- Carry forward H0's explicit disposition: the eight aliases with `S,S` in
  both modes form H1's initial supported set; DeepSeek V4 Pro remains
  unavailable pending Fireworks revalidation; both Meta aliases remain
  overlay records but are unsupported for native tool use pending clean
  requalification under the same bounded scenario.
- Prefer an authoritative small source or a deliberately owned craze registry
  over a broad catalog that needs a competing overlay.
- **Exit**: one panel-reviewed catalog decision, exact pin/provenance, all-11
  target diff, update strategy, support-status representation, requalification
  rule, and H1 input contract.

### H1 — skeleton

- `internal/harness`: home dir; the selected catalog plus overlay; Fantasy
  provider factory; store with header/message/model_change/effort_change
  entries; and a craze-owned `LanguageModel.Stream` loop for stream
  collection, manual assistant/tool history, complete-call continuation
  independent of finish reason, usage/finish accounting, event mapping,
  steering/history replacement, cancellation, and hard bounds.
- `internal/agent`: `NativeProvider()` with `hidden: true`; session adapter;
  `Capabilities{Effort, Modes, Interject}`; model dialog lists catalog
  models; effort as a config option.
- Refine H0's rough 1.5–2.5 kLOC direct-loop estimate and budget a comparable
  amount of scripted/local-stream fixture work before implementation.
- **Exit**: a conversation with all eight initially supported models from the
  real TUI via `--provider native`, mid-session model switch, cancel,
  interject, and `craze prompt --json` unchanged. The three retained-but-
  unsupported records are not selectable. Picker never shows the provider.

### H2 — tools

- PR 1: read, bash, edit, write with the truncation and kind wrappers, the
  edit ladder, per-file mutation queue, diffs through `textdiff`.
- PR 2: ls, glob, grep; doom-loop guard; length-stop rule; orphaned-tool
  closing on cancel.
- Testing pattern established: scripted `LanguageModel` and local streamed
  fixtures.
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
