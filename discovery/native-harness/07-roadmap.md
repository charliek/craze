# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one or two
PRs, gated per commit, closed by a mac-mini live smoke. The provider stays
hidden throughout (`01`). Sizes are guidance from the reference reviews.

| ID | status | one line |
|---|---|---|
| H0 | complete | Fantasy `v0.43.2` providers fit all four provider classes; `Agent.Stream` fits behind a finish-normalizing wrapper; Catwalk is not embedded |
| H1 | complete | skeleton shipped: hidden `native` provider; 14 of 15 imported models held a clean TUI turn, multi-turn sessions and `craze prompt --json` ran on a subset (OpenRouter via `openaicompat`, D-35); no interject, no modes (D-34) |
| H2 | complete | tools shipped across PRs #33, #34, #35, #37: opencode's ported contract (`read`, `write`, `edit`, `bash`, `grep`, `glob`, D-38), the gate seam allowing everything (D-39), ripgrep on `PATH` (D-41), the doom-loop guard (D-42), abnormal-finish handling (D-43), and interject; live smoke completed 14 of 15 imported models on Linux and 4 of 4 on the mac-mini; D-35's condition met, `openaicompat` stays |
| H3 | not started | approval, scheduled **after H8** (D-48): owner's direction is an auto-mode evaluator over the H2 gate, not ask-on-everything; scope decided when planned, after session-control S1 (D-39; Plan 019 §3.3) |
| H4 | complete | Claude compat and the shell mode shipped across PRs #39, #40, #43: content sources behind one seam with native discovery and the two projections (#39, `b861865`); the prompt-extras seam, the `@path` instruction loader with confinement, the model-facing catalog, and `[compat.claude]` toggles (#40, `c2940a6`); the composer shell mode, its process runner, and shell output carried to the agent with the next prompt (#43, `be05da9`); live smoke round-tripped on cursor and native, grok covered by `TestShellContextNeverReachesTheScreen` rather than driven live |
| H5 | complete | modes shipped across two PRs, #45 (`b0ea4c4`) and #48 (`feature/plan-023-h5-modes`): the three tools (`ask_user_question`, `exit_plan_mode`, `todo_write`), modes switched on for native, the plan file under the harness home, and the existing offer implementing an approved plan |
| H6 | complete | sub-agents shipped across three PRs: PR 1 (#51, `f5c3cfd`) end to end; PR 2 (#53, `5901e4a`) per-child stop; PR 3 (`feature/plan-026-h6-background`) background children — `run_in_background`, `agent_output`, the session-level wake, `bg` on the row |
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
  between steps (C12, reviewed alone, races); the live smoke, which found
  the schema-number bug below and was fixed before the smoke was recorded
  clean (C13); this docs update, recording the smoke tables and D-40's
  second table re-run (C14).
- **OpenRouter tool-loop check (D-35)**: run a multi-step tool loop on the
  OpenRouter reasoning models. If one degrades or fails without
  `reasoning_details` replay, first carry the replay in craze's own wrapper;
  the owner accepts `providers/openrouter` and its SDKs if quality needs it.
  The live smoke met the condition clean (below); `openaicompat` stays
  (D-44).
- **Exit**: the agent makes a real change in the craze repo, on Linux and
  on the mac-mini, with tool cards and diffs rendering as they do for grok.

**Exit result:** shipped across four PRs. PR 1 #33 (merged 0277ce7) landed
the store's tool-step support (`AppendStep`, the pairing invariant, tail
rollback) and the `internal/harness/tool` framework (`Tool`/`Prepared`/
`Spec`/`Result`/`Env`, the dispatcher, the gate, profiles, truncation and
spill files, per-path locks). PR 2 #34 (merged bc2aaab) shipped `read`/
`write`, `edit` (differentially checked against opencode under bun: 140,000
random cases and 9,000 execute-path cases agree), `bash`, and `grep`/`glob`
over ripgrep, behind the `opencode` profile. PR 3 #35 (merged ad89ef9) wired
the runner (`toolbridge.go`, per-turn state, ids, events, stop reasons,
cancel precedence, abnormal-finish handling), the doom-loop guard, the
adapter's tool cards and TUI golden frame, and the pytest tool-loop fixtures
(130 CLI tests, up from 121). PR 4 #37 added interject, the schema-number
fix the smoke found, and this docs commit; it is where the smoke ran.

Live smoke (2026-09-19), Task A (read → edit → bash → report, in a scratch
clone):

Linux — 14 of 15 imported models completed the loop and really changed the
file, in 8–35 s:

| alias | result |
|---|---|
| `fireworks/kimi-k3` | clean, file changed |
| `fireworks/kimi-k2p7-code` | clean, file changed |
| `fireworks/qwen3p8-max` | clean, file changed |
| `fireworks/deepseek-v4-flash` | clean, file changed |
| `fireworks/deepseek-v4-pro` | 404, stale wire id — the same H1 recorded, unchanged (D-24 follow-up) |
| `glm-5.3` | clean, file changed |
| `glm-5.3-flash` | clean, file changed |
| `muse-spark-1.3` | clean, file changed |
| `muse-spark-1.3-contributor` | clean, file changed |
| `openrouter/gemini-3.8-flash` | clean, file changed |
| `openrouter/glm-5.3-flash` | clean, file changed |
| `openrouter/gpt-5.6-luna` | clean, file changed |
| `openrouter/gpt-5.6-sol` | clean, file changed |
| `openrouter/gpt-5.6-terra` | clean, file changed |
| `openrouter/minimax-m3` | clean, file changed |

macOS (mac-mini), one model per provider class, all completed Task A and
changed the file, with ripgrep from Homebrew:

| alias | result |
|---|---|
| `fireworks/kimi-k3` | clean, file changed |
| `glm-5.3` | clean, file changed |
| `muse-spark-1.3` | clean, file changed |
| `openrouter/gpt-5.6-terra` | clean, file changed |

**D-35's condition is met** (D-44): every OpenRouter reasoning model ran a
multi-step loop, then a second call that consumed the first call's output,
then a switch to another provider — all clean; the same for Kimi K3 and
DeepSeek V4 Flash on Fireworks (the `reasoning_content` rule). No
`reasoning_details` replay was needed, so OpenRouter stays on `openaicompat`
and `providers/openrouter` is not taken.

Lifetime, live on Linux: a cancel during `sleep 321 & … wait` left neither
the shell nor its backgrounded child alive and the turn reported
`cancelled`; a child backgrounded past a normal exit
(`sleep 322 >/dev/null 2>&1 &`) was gone too. On the mac-mini the same check
was made decisive: both the shell and its backgrounded child were confirmed
alive before the cancel (2 of 2) and neither was alive after it (0), with
the turn reporting `cancelled`. An earlier macOS attempt was inconclusive —
it cancelled before the command had written its pid files — and is not
counted.

No disruption: `~/.craze/config.toml` and `~/.craze/sessions.jsonl` were
byte-identical (same MD5, still 21 rows) after about thirty native
sessions. Secrets: every artifact was grepped for all four resolved
provider keys and the canary — clean.

**What the smoke caught:** the first live tool call failed with Fireworks'
`Error validating JSON Schema: '9007199254740991' is not of type 'number'`.
C8's review fix had made schema numbers `json.Number` so the header's hash
would describe the exact literals sent, but a `json.Number` is a string
underneath and the provider's SDK wrote it quoted. Numbers are canonicalized
to int64/float64 once and the hash is taken of that same form: 2^53+1 still
goes out whole, the digest still describes the wire, and a literal's
spelling (1.0 → 1) is what was given up, along with an integer past int64,
which rounds. The tests that marshal the schema stayed green throughout,
because the standard library writes a `json.Number` unquoted and only the
provider's own encoder quotes it (checked against openai-go v3.54.0) — so
the guard is an assertion on the value handed over, that no `json.Number`
survives canonicalization, rather than on its JSON. Nothing in the repo
marshals through a provider SDK's encoder, which is the coverage this class
of bug really wants.

Known limitations: a refusal's text reaches the model always, but the card
shows it only for `read` and `bash` rows — `edit`, `write`, and search rows
show a failed row without it, because the TUI draws only their diff or
heading (§3.10 pins `internal/tui`); a call Fantasy refuses before dispatch
keeps Fantasy's own text on its card and the doom-loop guard still counts
and stops the turn on the fifth; Fantasy re-sends the model's own tool-call
arguments within a turn, so an argument the model wrote comes back to it
unredacted while the stored copy carries the marker; `bash`'s process-
lifetime edges (a stop between the pre-start check and launch, a syscall
goroutine that never returns, macOS reaping the leader before the final
group kill, a process that starts its own session escaping the kill);
`edit`'s two edge cases (a Unicode-normalized Home path, a kept BOM tipping
`replaceAll` over its 10 MiB cap); the dispatcher's own spill write being
synchronous on `Run`'s path. See `05-tools-and-permissions.md` for the full
list.

### H3 — approval

Reworded from "permissions" (Plan 019 §3.3): D-12 and `05`'s permission
model stay as written but lose their phase. The owner's direction is
**likely an auto-mode evaluator layered over H2's gate** (`Gate.Check`),
not ask-on-everything — a model or evaluation step flags only
dangerous-looking actions, perhaps through hooks — but the scope is decided
when this phase is actually planned, **after session-control S1** lands its
engine-owned ask registry (D-39; `10` Q6). Nothing in H2 reads or writes a
grants file.

**Scheduled after H8 (D-48).** The roadmap order is H4, H5, H6, H7, H8,
then H3: the phases that make the harness useful come before the phase
that constrains it. Native stays on H2's `AllowAll` gate until H3 lands.

- **Exit**: not yet defined; depends on the shape chosen when planned.

### H4 — Claude compat

Re-scoped by the owner on 2026-09-20 and planned as Plan 022 (D-45..D-48):
content is read live behind one seam (`contentSources`,
`internal/agent/sources.go`) instead of `craze import claude`; the
model-facing catalog lists commands alongside skills; lazy loading of
nested instruction files and `paths:` gating of rules are deferred out of
H4 (D-47); H3 (approval) moves after H8 (D-48). Plan 022 also carries one
TUI feature the owner asked for alongside it: a composer shell mode
(`!cmd`) for every provider, whose output is fed back to the agent as
context with the next prompt.

- Instruction loader with `@path` imports and confinement (no `paths:`
  gating, no lazy loading); workspace and user skills and commands;
  Claude's installed-and-enabled plugins; a model-facing catalog of skills
  and commands in the frozen system prompt; `[compat.claude]` toggles; a
  composer shell mode on cursor, grok, and native.
- **Exit** (Plan 022 §7/§8), restated:
  - this repo's `CLAUDE.md` is followed with no tool call needed to find
    it;
  - a workspace skill and a plugin command both appear in the menu and
    expand;
  - the model reaches a catalog file (a skill or a command) with no slash
    typed;
  - confinement holds — no file outside the chain's root or `UserRoot` is
    ever read;
  - the shell mode round-trips on all three providers.

**Exit result:** shipped across two PRs; a third (the composer shell mode)
is part of this plan but not of H4's own compat scope and has not merged,
so the phase stays **in progress** — see the status table above. PR 1
(#39, merged `b861865`) landed content sources behind one seam
(`contentSources`, `internal/agent/sources.go`), native discovery of the
workspace chain, the user root, and Claude's installed-and-enabled
plugins, the menu/catalog projections over one resolved list, and
expansion of a typed `/name` into the prompt for commands and skills. PR 2
(this commit, `feature/plan-022-h4-prompt`) landed the harness's
prompt-extras seam and its defensive renderer, the instruction loader with
`@path` imports and confinement, the model-facing catalog, the
`[compat.claude]` toggles, and the `prompt_sources` journal note.

Live smoke (2026-09-20), Linux, headless `craze prompt` rather than tmux —
every V-item here is about what reaches the prompt, not about the TUI.
Binary rebuilt from the branch tip, model `fireworks/kimi-k3` (H2-qualified):

| # | what | result |
|---|---|---|
| V1 | "what must run before every commit here?" in `craze` | pass — answered the gate command plus the conditional `make test-cli`, cited `CLAUDE.md` in its thinking, no tool call |
| V3 | name the `shedtest` skills from `../shed/cmd`, a subdirectory | pass — both skills and one absolute path; the chain walked from the subdirectory to the repository root, no tool call |
| V4 | a project command's `$ARGUMENTS` and `` !`cmd` `` splice | pass — one `command` event (`plugin: project`, "from this project"), `$ARGUMENTS` substituted, the splice reached the model literally and was quoted back verbatim; craze executed nothing (D-46) |
| V5 | reach a catalog file with no slash typed; resolve `${CLAUDE_PLUGIN_ROOT}` | pass — the model declined the entry it was misnamed toward, found the real neighbour on its own, read its file unprompted, and resolved the variable from the row's `Root:` |
| V6 | no disruption | pass — `native` absent from `--help` and from the unknown-provider error |
| V7 | `prompt_sources`; `--json` prints `command` and no new kind | pass — `prompt_sha256` identical to the transcript header's, 2 instruction documents, 27 catalog rows; `--json` unchanged apart from `command` |
| V10 | confinement, live: `@~/.ssh/…` and `@../outside.md` in a scratch repo | pass — both refused with the two diagnostics the plan specifies; zero canary bytes on disk, the literal import lines present, the document's own legitimate content present |
| V2 | `../roost` menu rows | deferred — it is a TUI-menu check, belongs with PR 3's smoke |
| V8 | mac-mini | deferred — the box is shared with the parallel session-control plan, and CI already runs macOS |

R1, first-turn prompt size:

| workspace | prompt bytes | ≈ tokens | instruction docs | catalog rows |
|---|---|---|---|---|
| `craze-plan022` | 17,439 | ~4.4k | 2 (`AGENTS.md` 74 B, `CLAUDE.md` 1,592 B) | 27 |
| `shed/cmd` | 42,123 | ~10.5k | 1 (`CLAUDE.md` 26,103 B) | 30 |

Both runs sit well inside §9's ceiling (the profile plus 96 KiB of
instructions plus 24 KiB of catalog); neither tripped the 64 KiB
diagnostic and shed's 26 KB `CLAUDE.md` loaded whole under the 32 KiB
per-document cap with no truncation marker.

R2 — does the model follow the catalog with no slash command typed?
Yes, on `fireworks/kimi-k3`: V5 is the measurement, choosing to read a
catalog entry and resolving `${CLAUDE_PLUGIN_ROOT}` from its `Root:` with
nothing beyond the prompt's own preamble telling it to. V1 and V3 are the
weaker form of the same result — answered from instructions and catalog
with no tool call at all. A second model's measurement belongs with PR 3's
smoke.

Where real commands stall: no real command was driven to the point of
stalling on a tool native lacks in this smoke, so this is a static
identification rather than an observed one. The evidence is the survey
from execution amendment X3: across the installed plugins and the three
repositories' workspace skills, 19 files reference sub-agents, 26
`run_in_background`, 42 `AskUserQuestion`, and 0 `WebFetch`/`WebSearch`.
H5 (`AskUserQuestion`, plan mode) and H6 (sub-agents) are what close it.

Two decisions made during execution carry forward into the rest of the
roadmap. The vendor denylist (`pdf`, `docx`, `xlsx`, `pptx`,
`skill-creator`) applies to native's own workspace and user sources only,
never to plugin entries, because every plugin installs under
`<home>/.claude` and the path test would otherwise match all of them
(X6). Confinement's threat model is a static hostile checkout: the
TOCTOU window between the containment check and the open is accepted and
stated on `confinedPath` itself, because closing it would need
`openat2`-style directory-handle walking that macOS has no equivalent of
(X19).

The two external review rounds per PR converge on one lesson about
security: redaction has to run on the assembled prompt, not on the fields
that feed it, because framing text reassembles a key across a boundary
that did not exist when a per-field redactor ran (X17); a key embedded in
an *identity* — a filename, a plugin id, a skill name — cannot be
redacted without breaking the thing it identifies, so the entry is
dropped or the session is refused instead (X23, X25, X26); and
diagnostics written before the redactor existed were their own leak,
because loader `warn` lines interpolate paths and import references
straight from the untrusted tree (X22).

PR 3 (#43, merged `be05da9`) landed the composer shell mode, its process
runner (`internal/tui/shell_run.go`, opencode's process lifecycle ported
rather than imported), and the path that carries a command's output to the
agent with the next prompt. **H4 is therefore complete.**

Live smoke (V9, Linux, 2026-09-20), real tmux, binary rebuilt from the
branch tip:

| what | result |
|---|---|
| round trip on cursor: `! git status --short`, `! echo SHELLOUT-ALPHA`, then "summarise that" | pass — the user row shows only what was typed; the agent's echo leads with the `<shell_context>` block, carrying both commands' output and exit status |
| the same on native, real model `fireworks/kimi-k3` | pass — `!echo NATIVE-SHELL-BETA` then "What exactly did that command print?"; the model answered `NATIVE-SHELL-BETA`, the user row showed only the question |
| `!sleep 300` then Esc | pass — killed, no `sleep 300` left on the box |
| `!sleep 4242 & sleep 4243` then Ctrl+C | pass — both gone, the backgrounded child included, and craze kept running: no quit, no turn cancelled |
| `!sleep 5150 & sleep 5151` then Ctrl+D | pass — craze exited and nothing survived |

Grok was not driven live: the fake agent's `echo` script advertises no auth
method grok accepts (`agent: no supported auth method`), and the real
`grok` binary is the one recorded as returning 402 in plan 012's smoke.
What covers grok instead is that the mechanism is provider-independent —
`SplitShellContext` and the attachment both happen in the TUI, before the
text reaches any session — and `TestShellContextNeverReachesTheScreen`
runs one sub-test per provider (cursor, grok, native), each over the own
row, a drained row, a replayed row, the interjection echo, the queue band,
and the title. The real-terminal proof exists on two of the three
providers; the smoke record is explicit that this is not the same as
driving three live.

V2 (the `../roost` menu check) is still not run: PR 3's smoke did not
include it. V8 (mac-mini) is still not run either, for the reason already
recorded — the box is shared with the parallel session-control plan and CI
already runs macOS.

### H5 — modes

Unblocked and next: H4 is complete, and Plan 021's PR 2 — the dependency
this phase was gated on — has merged as `db7686e`.

- Plan, ask, implement as rulesets + reminders; dispatcher enforcement;
  `exit_plan_mode` and `ask_user_question` tools; `todo_write`.
- **Exit**: a plan-mode round trip with the plan card and accept →
  implement; a question card answered; todos in the tasks panel.

**Planned (Plan 023, 2026-09-21):** FINAL after panel review. Two PRs, both
auto-merged: PR 1 (`feature/plan-023-h5-harness`) is the harness side and
the asker seam — the mode gate, the plan file, reminders, `todo_write`,
`ask_user_question`, `exit_plan_mode`, and native opening asks and carrying
todos; PR 2 (`feature/plan-023-h5-modes`) switches modes on in the adapter,
the CLI, and the TUI, and starts only once Plan 021's PR 3 is on
`origin/main`. The three tools keep grok-build's ids
(`ask_user_question`, `exit_plan_mode`, `todo_write`), each description
opening with its Claude Code name as an alias. Decisions: D-49 (a mode is
a gate over a canonical target, the toolset stays fixed), D-50 (the plan
file, read by the tool, never argued), D-51 (approving ends the turn
through a typed handoff; the TUI's offer implements), D-52 (no ask
timeout), D-53 (the tool ids and their Claude-name aliases).

**Exit result:** shipped across two PRs. PR 1 (#45, `b0ea4c4`, 10 commits
preserved) landed the harness side: `Options.Mode`, `Session.SetMode`/`Mode`,
`tool.ModeGate` over `AllowAll` with the plan and ask rejection texts,
`Request.Targets` for edit-kind tools, the plan file (`store.PlanPath`) and
`mode_change` at step boundaries, reminders spliced in from step 0 with
full/sparse alternation, `todo_write` (harness-owned list, merge/replace, the
auto-upgrade, caps), `ask_user_question` and `exit_plan_mode` (grok-build's
ids and outcome texts, each description opening with its Claude Code name as
an alias), the typed approval handoff that ends a turn from a tool, and
native opening asks through `tool.Asker` with todos projected to the tasks
panel. PR 2 (`ab351a7`, review fixes `ae16ad2`) switched modes on: `SetMode`
on native the way Plan 021's C10 left the setters, `Snapshot.Modes`/
`CurrentMode`, `refuseInProcess` keyed on `Capabilities.Modes` (so `craze
--provider native --plan` works in the TUI path too), the plan-offer made
eligible for a turn with no assistant text, and the question card's
description line.

Live smoke, Linux, real tmux, binary rebuilt from each PR's smoke tip (PR 1
`8993e7d`, PR 2 `ab351a7` — the review fixes after it, `ae16ad2`, changed
no behaviour a smoke exercises),
`fireworks/kimi-k3` and `openrouter/glm-5.3-flash`, `--provider native`
(PR 1: `023-native-harness-h5-modes/smoke/pr1-linux.md`; PR 2:
`smoke/pr2-linux.md`):

| # | what | result |
|---|---|---|
| V1 | `/plan`, plan a change, accept, offer, Enter | pass (kimi, the whole path): the plan file written under the home, `⟳ ask present the plan`, the card, `a` → accepted, the turn ended with no assistant text, the offer armed, Enter → agent mode, "Implement the plan above.", the file changed; glm ran it through the accept and the armed offer only |
| V2 | in plan mode, an outright edit of a repo file | no denial fired live on either model: both kimi and glm obey the plan reminder and present a plan instead of attempting the edit; the live denial is V7's; the plan-mode denial of a non-plan file is covered by the harness tests and the `native-plan-denied` golden |
| V3 | a skill that asks a question (`AskUserQuestion` by Claude's name) | pass: the card raised (two options, `1-9 pick`), an option picked, the transcript row read the answer, the model used it |
| V4 | reject the plan; feedback as the next message | pass (kimi): rejected, the model asked what to change, the next message produced a revised plan |
| V5 | Esc on the plan card | pass (kimi): cancelled, chip stayed `plan`, `/agent` left |
| V6 | Ctrl+C while the question card is open | pass: cancelled, the TUI stayed responsive, the next prompt ran |
| V7 | Shift+Tab mid-turn while the model is writing files | pass (kimi), richer than planned: Shift+Tab cycled agent→plan→ask mid-turn; a write of the plan file the model had placed while told "plan" ran after the switch to ask and was denied (`Rejected: ask mode is read-only…` — the Prepare→SetMode→Run boundary, live), so was the next workspace write, `exit_plan_mode` in ask mode was denied by name, and the model was told at the next step boundary |
| V8 | `craze prompt --provider native --plan/--ask --json` | pass on both models, both halves: `--plan` ends with the plan auto-accepted and no workspace file changed; `--ask` denies every non-read-only call and answers from the reminder |
| V9 | todos: a multi-step task | pass: the tasks panel filled, statuses moved, `TASKS 2/2 ✓` |

R1: PR 1 already had `TodoWrite` and `AskUserQuestion` reaching their
snake_case tools; PR 2 added `ExitPlanMode` — **3 of 3 Claude names now reach
the tool on both models**, with no hint beyond the alias sentence. R2:
neither model writes todos unprompted on a multi-step task (measured in PR
1's smoke; PR 2 did not rerun it);
the prompt-sentence follow-up stands. **R3, the cross-turn cache cost,
measured on `fireworks/kimi-k3`:** plan mode is a full cache miss on every
turn's first request (0 cache-read tokens, against ~9,000–10,000 in agent
mode on the same turns) — worse than §3.3's stated model, which expected the
system-prompt prefix to hit and only the tail to diverge; within a turn the
second request still hits on both. The variant-marker replay of reminders is
now the priority follow-up, not a nice-to-have.

**This is the first time a real model called `exit_plan_mode`, and ending
the turn from the tool behaved exactly as designed**: the tool result was the
approval text, the turn ended `end_turn`, the mode stayed `plan`, and the
TUI's offer armed. D-51 holds and §3.4's fallback was never needed.

V10 (the mac-mini): not run; CI runs macOS.

Follow-ups:

- the variant-marker replay of reminders (priority, R3);
- OWNER'S CALL: a native ask parked at quit ends `cancelled, by call`, not
  `closing` (X10; adapter-only change if wanted);
- PRE-EXISTING: `native_tools.go`'s tool-row projection sanitises after
  redaction with no second redaction (X15);
- PRE-EXISTING: `announceCurrent` (`SetModel`/`SetConfig`) has the same
  Close window `announceMode` now guards (X23.1);
- PRE-EXISTING: bubbletea v1 treats a fast chunk of runes spelling a key
  name as that key in the composer (X24);
- cosmetic: a `todo_write` that only completes items titles its row `0
  todos` (X17);
- free-text "Other" answers; `enter_plan_mode`; the todo prompt sentence
  (§4, §9; `10-open-questions.md` already lists these three);
- `--json` has no field for a plan's body (X22.7).

H6 (sub-agents) is complete: PR 1 (#51, `f5c3cfd`) shipped the in-process
`agent` tool, event tagging, and personas from workspace, user, and plugin
sources; PR 2 (#53, `5901e4a`) added per-child cancel; PR 3
(`feature/plan-026-h6-background`) adds background children —
`run_in_background`, `agent_output`, and the session-level wake that
delivers a result nobody typed for.

### H6 — sub-agents

**FINAL after the S1c seam review and the panel (2026-09-24); shipped.**
Three PRs, each auto-merged after `/git-commands:watch-pr` shows it green:
PR 1 (`feature/plan-026-h6-subagents`, foreground sub-agents end to end)
**merged as #51, `f5c3cfd`**; PR 2 (`feature/plan-026-h6-stop`, per-child
cancel) **merged as #53, `5901e4a`**; PR 3 (`feature/plan-026-h6-background`,
background children and the wake) as built, on
`feature/plan-026-h6-background`. Decisions D-54..D-59.

- **In-process** (D-54, supersedes D-10): a child is a second
  `harness.Session`, as grok-build, opencode, codex and crush all run their
  own sub-agents. This closes Q8. The latency numbers that were to decide
  it, measured on this plan's `9125ec7` baseline
  (`026-native-harness-h6-subagents/latency/`):

  | | Linux | mac-mini |
  |---|---|---|
  | local start before the first provider byte (median, n=10) | 18.5 ms | 7.7 ms |
  | one fresh TLS handshake | 80–145 ms | 80–145 ms |
  | resident memory per child | ~25 MB | ~16 MB bare start |
  | time to first token, for comparison | 0.65–2 s | 0.65–2 s |

  The decision rested on surface, orphans, live tokens and per-child
  cancel, not on latency; the process path stays behind the runner's seam,
  triggered only by a child crash or leak seen in practice, or
  session-control S4 wanting children as separately attachable hosts.
- The `agent` tool (kind `task`, D-55); personas from three sources in their
  own namespace (D-56); a child that inherits the parent's mode and is only
  ever tightened by a later switch, never loosened (D-57); model and effort
  resolved per call through an optional `[subagents]` tier map (D-58);
  per-child cancel in PR 2, and background children — a session-level wake
  through the existing foreign-turn machinery — in PR 3 (D-59).
- **Background children (PR 3, D-59 as shipped).** `run_in_background`, off
  by default, on only for an interactive session (`Options.Interactive`) —
  headless `craze prompt` always runs a background call in the foreground.
  A background call takes one of the same four slots but never waits: it
  fails fast when none is free, where a foreground call still waits behind
  another foreground holder. A finished result is redacted and capped like
  a foreground answer, then delivered at the next step boundary, by the new
  `agent_output {id, wait_ms}` tool, or by the session's own **wake**: one
  worker per session that starts a turn of its own — bracketed as
  `EventForeignTurn{Reason: "subagent_wake"}`, fenced by the new
  `agent.AdmissionFence` so the engine never admits a craze prompt while it
  runs — when nothing else holds the session's claim. The row band marks a
  background child `bg` (`Capabilities.SubagentBackground`, native only). A
  drained row refused by the wake is restored at the queue's head (SF-21,
  widened to every row-sourced turn a foreign turn refuses).
- **Exit**: a task fanned out to two children with both transcripts in the
  sub-agent view.

**Exit result:** the criterion above was met live, on both Linux and the
mac-mini (PR 1's V1). PR 2's per-child stop was verified live on both
platforms (V9). PR 3's background children — `run_in_background`,
`agent_output`, the wake — are built and gated (§3.11's re-verification,
the literal moves, the new goldens) and verified live on both platforms
(V10).

Live smoke, all three PRs (plan artifacts, outside the repo — see
`026-native-harness-h6-subagents/smoke/pr{1,2,3}-*.md`):

| # | what | platforms | result |
|---|---|---|---|
| V1 (PR 1) | exit criterion: two children fanned out, both transcripts in the sub-agent view | Linux, mac-mini | pass |
| V3 (PR 1) | Esc mid-fan-out kills both children's `sleep 30`, nothing left running | Linux, mac-mini | pass |
| V7 (PR 1) | `craze prompt --json`: subagent and child-tagged lines, `done end_turn` | Linux, mac-mini | pass |
| V9 (PR 2) | Delete/Backspace stops one running child, from its row and from its view, native only; grok's row and view fall through with no `del to stop` hint | native: Linux, mac-mini; grok fall-through: Linux | pass |
| V10a (PR 3) | a background `agent` call returns at once; `bg` marks the row while it runs | Linux, mac-mini | pass |
| V10b (PR 3) | a background child's finish during the parent's own turn is steered into its next step | Linux, mac-mini | pass |
| V10c (PR 3) | a background child's finish while the session is idle wakes the parent once, heading the reply with "sub-agent finished — the agent continues" | Linux, mac-mini | pass |
| V10d (PR 3) | a prompt typed while the wake runs queues, and sends once the wake ends | Linux, mac-mini | pass |
| V10e (PR 3) | `agent_output` waits on a running background child and returns its result; no wake follows | Linux | pass |
| V10f (PR 3) | Esc while idle leaves a background child running; Esc during a wake stops the wake | Linux | pass |

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
background bash with auto-background · the process-based sub-agent runner
(D-54's fallback, triggered by a crash/leak or S4) · sandboxing ·
flipping the provider to visible · lazy loading of nested instruction files
(deferred out of H4 by D-47) · `paths:` gating of rules (deferred out of H4
by D-47).

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
