# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one or two
PRs, gated per commit, closed by a mac-mini live smoke. The provider stays
hidden throughout (`01`). Sizes are guidance from the reference reviews.

| ID | status | one line |
|---|---|---|
| H0 | not started | spike: Go 1.27, fantasy, catwalk in; every gx model through a three-step tool loop; decisions recorded |
| H1 | not started | skeleton: home dir + env override, catalog overlay, provider factory, re-read loop over the JSONL tree store, hidden native provider, effort option, cancel, interject |
| H2 | not started | tools: read, bash, edit, write, then ls, glob, grep; truncation wrapper; kind metadata; edit diffs; doom-loop guard; scripted-model test harness |
| H3 | not started | permissions: once / always / reject-with-feedback, prefix table, dangerous list, per-workspace grants, Claude rule syntax, yolo |
| H4 | not started | Claude compat: instruction files with imports and `paths` gating, workspace skills and commands, `craze import claude` for global instructions, user skills, and marketplace plugins |
| H5 | not started | modes: plan mode in the dispatcher, exit-plan and question tools, todos |
| H6 | not started | sub-agents: child-process agent tool, depth 1, derived permissions, personas from workspace and imported agents |
| H7 | not started | resume and compaction over the store, `--continue`/`--resume`/rename, cost in the status row |
| H8 | not started | images: clipboard read per OS, composer attachments, vision flag strip |

## Phase detail

### H0 — spike
- Bump Go to 1.27 in `.mise.toml`, `go.mod`, CI, goreleaser; add fantasy and
  catwalk.
- Throwaway `main` under `/tmp` or a scratch branch: for each model in the
  gx set, run a scripted three-step tool loop; record streaming tool-call
  behaviour, effort field acceptance, image rejection, reasoning replay.
- **Exit**: a table in `04` of pass/fail per model per quirk; D-01…D-05
  confirmed or amended.

### H1 — skeleton
- `internal/harness`: home dir, catalog (catwalk embedded + overlay + gx
  TOML reader), provider factory, store with header/message/model_change/
  effort_change entries, turn runner (text and thought only, no tools),
  cancel, `PrepareStep` steering.
- `internal/agent`: `NativeProvider()` with `hidden: true`; session adapter;
  `Capabilities{Effort, Modes, Interject}`; model dialog lists catalog
  models; effort as a config option.
- **Exit**: a conversation with every gx model from the real TUI via
  `--provider native`, mid-session model switch, cancel, interject, and
  `craze prompt --json` unchanged. Picker never shows the provider.

### H2 — tools
- PR 1: read, bash, edit, write with the truncation and kind wrappers, the
  edit ladder, per-file mutation queue, diffs through `textdiff`.
- PR 2: ls, glob, grep; doom-loop guard; length-stop rule; orphaned-tool
  closing on cancel.
- Testing pattern established: scripted `LanguageModel`, VCR cassettes.
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
Meta direct overlay (one config entry, do when needed) · background bash
with auto-background · in-process sub-agents · sandboxing · flipping the
provider to visible.

## Conventions per phase

- Plan first (panel review), then `flows:gauntlet` or `flows:gated-commit`.
- `make lint && make test && make build` per commit; `make docs` only if
  `docs/` changes (these files are not `docs/`).
- Update `08-decisions.md` when a phase changes a decision; update the
  status column above when a phase merges.
