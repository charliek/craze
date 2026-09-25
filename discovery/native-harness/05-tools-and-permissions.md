# 05 — Tools and permissions

## Tool set

H2 ports opencode's contract (commit `5f9d9187`, MIT) rather than inventing
one: ids, parameter names and types, required lists, model-facing
descriptions, limits, and result phrasing, with attribution in
`internal/harness/tool/opencode/NOTICE` (D-38). This supersedes D-06's
four-tools-then-`ls` plan and its multi-edit shape.

| tool | kind | parallel | limits and behaviour |
|---|---|---|---|
| `read` | read | yes | `filePath`, `offset?` (1-indexed), `limit?` (default 2000); lines as `<n>: <content>`; 2000 lines, 2000 chars per line, 50 KiB cap; `<path>`/`<type>`/`<content>` envelope; directories listed with a trailing `/`, sorted (there is no separate `ls` tool); binary refused by extension list, NUL byte, or >30% non-printable in a 4096-byte sample; "Did you mean" suggestions on a missing file; images and PDFs refused (H8) |
| `write` | edit | no | `filePath`, `content`; overwrite, parent directories created (`MkdirAll` 0755 before the atomic write); BOM preserved; result `Wrote file successfully.`; temp-file-and-rename under a path lock (see Shared rules) |
| `edit` | edit | no | `filePath`, `oldString`, `newString`, `replaceAll?`; nine replacers tried in order (simple, line-trimmed, block-anchor, whitespace-normalized, indentation-flexible, escape-normalized, trimmed-boundary, context-aware, multi-occurrence); an ambiguous hit falls through to the next replacer; a match much larger than `oldString` is refused; CRLF and BOM preserved; empty `oldString` only creates a new file; result `Edit applied successfully.`; the diff goes to `Result.Edits` only — **the adapter**, not the tool, computes the card's diff with `internal/textdiff` (the tool package may not import it, §"Seams" below); known limitations: a Unicode-normalization spelling of Home could name `providers.toml` only while Home does not exist (a live session rules this out), and a kept BOM can put a `replaceAll` result three bytes over its 10 MiB cap |
| `bash` | execute | no | `command`, `timeout?` (ms), `workdir?`; `/bin/bash -c`, else `sh -c`; `Setsid` (new session, no controlling terminal); stdin `/dev/null`; stdout/stderr merged; default timeout 120 s, cap 600 s (a larger request is clamped and the result says so); everything left in the command's session is killed when it returns; a non-zero exit is **not** an error result — the model reads the code; live output streams through a harness-owned progress channel, throttled to 100 ms, lossy; tail kept at 2000 lines / 50 KiB by the tool itself, full output spilled once it passes 50 KiB; `exit code: N` added to `<shell_metadata>` on a non-zero exit; known limitations: a stop landing between the pre-start check and launch taking the started command kills it without the TERM grace, a goroutine stuck in a syscall that never returns leaks until it returns (bounded memory), macOS reaps the leader before the final group kill (the same window `internal/acp/spawn.go` accepts), and a process that starts its own session or process group escapes the kill |
| `grep` | search | yes | `pattern`, `path?`, `include?`; ripgrep (`PATH`), `--hidden`, `.gitignore` respected, 100 matches, grouped by file as `  Line n: text` |
| `glob` | search | yes | `pattern`, `path?`; ripgrep `--files --glob`, no `--hidden`, 100 results, ripgrep's own order |
| `todo_write` | todo | yes | harness-owned list; `merge` (default true) patches by id, an explicit `merge:false` replaces unless the auto-upgrade applies; caps at 64 items × 200 bytes, UTF-8-truncated; every write feeds one full-list `EventTodos` |
| `ask_user_question` | ask | no | blocks on the person; feeds `EventQuestion`; no timeout (D-52) — cancelled, closed, or turn-ended returns grok-build's unanswered text — error class `aborted` when the call was stopped (its context done or `Env.Closing` closed), otherwise a non-error result |
| `exit_plan_mode` | ask | no | blocks on the person; reads the plan from the plan file through the file tools' guarded open, never from arguments; approve ends the turn (D-51), reject continues plan mode with no feedback channel, Esc cancels the turn with the mode unchanged; empty or missing file → `EmptyPlan`, no ask |
| `agent` | task | — | H6; see below |

Dropped from the earlier plan: **no `ls` tool** (`read` lists directories)
and **no multi-edit** (owner decision 1, D-38 supersedes D-06).

## Shared rules

- **No read-before-edit enforcement.** Prompt-only (D-12, amended by D-42
  only for the doom loop, not this rule). opencode's `edit.txt`/`write.txt`
  claim it is enforced; that claim is not ported (§3.2 of Plan 019).
- **Truncation is shared**, not written per tool: the dispatcher keeps the
  head of a long result (`read`, `grep`, `glob`) at 2000 lines / 50 KiB,
  and only when a limit is hit does it write the full text to
  `<Home>/tool-output/tool_<id>` and say where. `bash` is the exception: it
  truncates its own stream (it asks the dispatcher for no truncation), keeps
  the tail, and spills beyond 50 KiB itself, through the same spill-file
  rules — this keeps `bash`'s spill off the critical path, unlike the
  dispatcher's own spill write, which is synchronous on `Run`'s path, so a
  hung filesystem there can still hold up a tool result. `<Home>` is the
  harness directory `~/.craze/native/`; the spill directory is created 0700
  and used only through a handle opened on it after checking it is a real
  directory, so swapping in a symlink cannot redirect a write; files are
  0600, `O_EXCL|O_NOFOLLOW`, never overwritten (a taken name gets a random
  suffix; the model can still reach the directory through `bash`). Swept at
  `Open` for files older than 7 days; no background timer.
- **Known limitation, card text:** a refusal's text always reaches the
  model, but the card shows it only for `read` and `bash` rows — `edit`,
  `write`, and search rows show a failed row without it, because the TUI
  draws only their diff or heading and §3.10 pins that `internal/tui` does
  not change for this.
- **Parallelism is Fantasy's own**: a semaphore of 5 for parallel tools
  (`read`, `grep`, `glob`); non-parallel tools (`bash`, `edit`, `write`)
  serialize against each other but do not hold up parallel calls already
  started. No tool can force a whole batch sequential (Fantasy has no such
  hook — this corrects the earlier "a tool can declare itself sequential"
  claim). `edit` and `write` additionally take a per-path lock keyed on the
  resolved absolute path.
- **Tool ids are stable.** They key events, TUI rows, and spill file names;
  renaming one is a migration.
- **Descriptions are opencode's `.txt` files**, rendered once per session
  with `${name}` substitution via `strings.NewReplacer` — **never**
  `text/template` (D-37: the link test fails the build on any route to
  run-time method lookup). Each edit to a description's text is listed in a
  table in `NOTICE`.
- **File tools** resolve symlinks first, refuse anything that is not a
  regular file or a directory (a FIFO or device read ignores `ctx` and
  would hang `Close`), and lock on the resolved absolute path.

## Seams

Three seams carry the tuning and per-model work the owner expects (D-38):

1. **Tools ≠ Fantasy.** `Tool` is craze's own interface
   (`internal/harness/tool/**`); `toolbridge.go` is the only file that
   adapts it to `fantasy.AgentTool`. `internal/harness/tool/**` may import,
   of craze, only `internal/atomicfile` and `internal/harness/redact`, and
   must not link Fantasy even transitively — enforced by a depguard
   sub-rule and a `go list -deps` test. This is also why `edit`'s diff is
   computed by the adapter, not the tool: `internal/textdiff` is off limits
   inside `internal/harness/tool/**`.
2. **Profiles.** A `Profile` is `{Name, Tools, System}`, selected once per
   session at `Open` from the starting model (`ProfileFor`) and recorded in
   the transcript header (`tool_profile`, `tools_sha256`). H2 registers one
   profile, `opencode`. A later profile (per-model prompts, `apply_patch`
   for GPT-5-shaped models) is a new registration, not a rewrite.
3. **The gate.** `Gate.Check(ctx, Request) (Decision, error)` returns
   `Allow`, `Deny{Reason}`, or `Ask{Prompt}`. See "Permission model" below.

## Kind metadata

Each tool call carries the vocabulary craze's cards already read from grok:
`kind` (read, edit, execute, search, think, other, ask, todo, task — `ask`
and `todo` added in H5, Plan 023 §3.4; `task` added in H6, Plan 026 §3.3,
correcting this document's earlier "think" guess for `agent`), `read_only`,
and a
canonical input projection (`path`, `command`, `pattern`, `cwd`, …); bulky
edit bodies stay in `RawInput`, capped at 512 B. `ToolEvent.Kind`, `Title`,
`Locations`, `Output`, and `Diffs` are filled from this in the adapter,
which maps the harness's own events (`ToolStarted`, `ToolCalled`,
`ToolProgress`, `ToolFinished`) onto `ToolEvent` (D-17; §3.10 of Plan 019).

## Permission model (deferred design, D-39)

The design below is kept as the H3-era shape, but it has **no phase**: H2
ships a gate that allows everything, and the doom loop (below) is the only
guard that stops a turn. Nothing in H2 reads or writes a grants file. The
owner's likely direction for H3 is an auto-mode evaluator layered over this
same gate, not ask-on-everything, decided once session-control's S1 phase
lands (`07`, `10` Q6).

Three answers, and the pattern is computed by the tool, not the UI:

| answer | effect |
|---|---|
| once | run this call |
| always | remember `{tool, pattern}` for this workspace in `~/.craze/native/permissions/<cwd-slug>.toml`; for bash the pattern is a command prefix from an arity table (`git commit *`, `npm run *`), for files a path glob |
| reject | fail the call; optional feedback text goes back to the model as the tool result; every other pending ask in the session is rejected too |

- Evaluation is last-match-wins over: built-in read-only allowances, the
  workspace grant file, in-session grants. Default `ask`.
- A **dangerous list** (`rm`, `chmod`, `chown`, `kill`, `git push`, …)
  never honours a remembered prefix.
- Compound commands are split on `&&`, `||`, `;`, `|`; every segment must
  pass.
- **Rule syntax accepts Claude Code's spelling** (`Bash(git *)`,
  `Edit(src/**)`, `Read(*.env)`) so an imported or hand-written rule means
  the same thing in both tools (D-09). Tool-name aliases: `Bash`→`bash`,
  `Read`→`read`, `Edit`/`Write`→`edit`/`write`, `Glob`→`glob`,
  `Grep`→`grep`, `Agent`/`Task`→`agent`.
- `Force` (yolo) removes the permission wrapper entirely; the dangerous
  list still applies only through explicit deny rules.

### Doom loop (H2 has this now — D-42, amends D-12)

The guard counts in `OnToolCall`, in call order, and sees invalid and
unknown-tool calls too. The signature is the tool name plus canonical JSON
(object keys sorted; the raw string when the JSON is invalid); "consecutive"
means consecutive in call order across steps.

- The 3rd and 4th consecutive identical calls are not run; result class
  `doom_loop`: a nudge telling the model to stop repeating the call.
- The 5th gets the same result with a note that the turn is being stopped;
  the turn ends with stop reason `max_turn_requests`.
- Once an approval channel exists, the 3rd call becomes an `Ask` instead
  (opencode's own behaviour).
- **Known limitation:** a call Fantasy refuses before dispatch (invalid
  arguments, an unknown tool) keeps Fantasy's own text on its card instead
  of craze's; the doom-loop guard still counts it and still stops the turn
  on the 5th.

### Secrets (accidental disclosure only)

H2 makes no security claim: a model with `bash` can read anything the user
can and can defeat any value match with `base64` or `cut`. What H2 does
claim is that craze's own provider credentials do not reach the model, an
event, the transcript, or a spill file **verbatim and by accident**:

- A redacting replacer (`internal/harness/redact`) covers every loaded
  provider key, exact values only, applied once in the dispatcher to every
  outward-facing field (`Result.Text`, `Content`, `Output`, edit diffs,
  request fields as they appear in events) and to bash output before the
  progress snapshot and the spill file see it.
- `edit`/`write` refuse input containing the redaction marker (a poisoning
  guard: the model must re-read the file rather than write the marker back).
- File tools refuse `<Home>/providers.toml` by resolved path.
- The bash child's environment drops every variable named as an `env_key`
  in `providers.toml`, plus `OPENAI_*`. Other credentials (`GITHUB_TOKEN`,
  `AWS_*`, `SSH_AUTH_SOCK`) stay, because `gh`, `git push`, and cloud CLIs
  are the point of a shell tool; an allowlist was considered and rejected
  for that reason.
- A provider key the redactor cannot handle is refused for every provider,
  used or not: one under 8 bytes, which would shred ordinary text, and one
  that overlaps the redaction marker (`credential`, say, which the marker
  contains), which the marker would print back. An inline `api_key` is
  refused when `providers.toml` loads; a key from the environment when a
  session opens, where `Table.Keys` gathers every provider's keys for the
  redactor; and the llm factory still refuses a short key at use.
- **Known limitation:** Fantasy re-sends the model's own tool-call arguments
  within a turn, so an argument the model wrote comes back to it unredacted,
  while the copy in the store carries the marker.

## Modes

Modes are **rulesets plus prompt reminders**, never separate loops and
never a different toolset (D-49): the tool list stays fixed from `Open` to
`Close`, and every tool stays advertised in every mode. Enforcement is in
the **dispatcher** only, so it survives yolo: `tool.ModeGate` wraps
`AllowAll` (H3 later wraps its evaluator the same way).

| mode | what the gate does | extra |
|---|---|---|
| agent | everything reaches `inner` | — |
| plan | an edit-kind call is allowed only when its canonical `Targets` is non-empty and every entry equals the plan file's canonical path; otherwise denied. `bash`, `read`, `grep`, `glob` all reach `inner` — the dispatcher cannot see inside a shell, so the reminder carries the rule (owner decision 3). `exit_plan_mode` is available | plan reminder, full or sparse, appended as a synthetic user part |
| ask | anything not `ReadOnly` is denied | ask reminder; `exit_plan_mode` denied by name |

In every mode but plan, `exit_plan_mode` is denied by `Request.Tool` — it
is `ReadOnly` and would otherwise run anywhere and answer `EmptyPlan`.
`ask_user_question` and `todo_write` are allowed in every mode. Three of the
result texts are craze's own, not grok-build's, because grok-build's
wording would be false here: `EmptyPlan` names the plan file and says to
write the plan first (grok-build's says the mode is exited, which does not
happen); a plan nobody decided on gets "The user did not decide on the
plan. Plan mode is still active…" rather than the question tool's declined
text; the approved text is craze's own, "Your turn ends here; the user will
start implementation." (§3.4). The rejection texts are verbatim:

- plan, a denied edit: "Rejected: file edits are not allowed in plan mode -
  the only editable file is the plan file (`<path>`)."
- ask, a denied non-read-only call: "Rejected: ask mode is read-only - no
  edits, writes, or shell commands."
- any mode but plan, `exit_plan_mode`: "Plan mode has been disabled. Do not
  call exit_plan_mode again unless the user explicitly asks to re-enter
  plan mode."
- ask reminder text: "Ask mode is active: answer from what you can read. Do
  not edit or write files, and do not run shell commands — every such call
  is denied."

**`Request.Targets`** is the canonical, unredacted enforcement target — set
by `Prepare` for edit-kind tools, absolutised against the workspace, and
resolved through the same walk `file.go:86` uses (the longest existing
prefix through symlink resolution, the missing tail appended cleaned), so a
missing target, a dangling symlink, and an alternative spelling all
normalise the same way the eventual open will. It is separate from
`Paths`, which is cleaned but not symlink-resolved and is redacted before
the gate sees it, staying event-facing only. An edit-kind call with no
`Targets` is denied. The remaining race between the check and the open is
stated on `ModeGate` and accepted, as H4 accepted its confinement TOCTOU.

**Reminders** are synthetic user parts, inserted by `prepareStep` from step
0, never stored, never emitted as an event, and never rendered by the TUI;
full and sparse variants alternate per turn, and every plan reminder — full
and sparse alike — carries the plan file's absolute path, because history
is rebuilt from the store, which omits reminders. A turn's reminder is in
every request of that turn but not in the next turn's history, so a plan-
or ask-mode turn's first request diverges from the previous turn right
after the previous prompt and the provider re-reads it uncached — stated
and measured (H5 R3), not fixed; a one-byte variant marker on the stored
user entry is the follow-up if it matters. Measured on `fireworks/kimi-k3`:
a plan-mode turn's first request is a **full** cache miss (0 cache-read
tokens) against ~9,000–10,000 in agent mode on the same turns — worse than
the model above, which expected only the tail past the previous prompt to
diverge; within a turn the second request still hits on both. The
variant-marker replay is now the priority follow-up, not a nice-to-have.

The plan file is the transcript's sibling,
`<home>/sessions/<slug>/<stamp>_<id>.plan.md` — the only writable path in
plan mode. `exit_plan_mode` reads it through the file tools' guarded open
(non-blocking, regular file only, bounded at 256 KiB), never from an
argument. **No ask timeout** (D-52): a cancelled, closed, or turn-ended ask
returns grok-build's unanswered text — as an error of class `aborted` when
the call was stopped (its context done, or `Env.Closing` closed), and as a
non-error result otherwise, an ask that was skipped or left unanswered while
the turn lived. Approving the
plan **ends the turn** through a typed handoff, not `StopTurn` (D-51); the
TUI's existing "implement" offer then switches to agent mode and sends the
provider's implement prompt exactly as craze does for cursor and grok.
Rejecting continues the turn in plan mode with no feedback channel — the
user's feedback is their next message.

**The veto after approval is decided by a call's place in its step**, not by
when its goroutine reaches the turn's lock: every call ordered after the
approving `exit_plan_mode` in Fantasy's own dispatch order is refused,
whether or not it had started running, so a read placed and dispatched
before the ask is never refused just because its goroutine had not yet
reached the check. Execution found the timing-based rule unsound (a
scheduling race could refuse a call placed before the ask) and fixed it
before the live smoke ran. Telling the model of a mode change and writing
its `mode_change` store entry both wait for a step that persisted output —
a reasoning-only or cancelled step does not consume a pending transition, so
it survives to the next boundary that does.

## Sub-agents (H6)

**Status: shipped (Plan 026, FINAL after the panel, 2026-09-24; PR 3
2026-09-25).** Three PRs — foreground sub-agents end to end (#51,
`f5c3cfd`), per-child cancel (#53, `5901e4a`), background children
(`feature/plan-026-h6-background`). See `07`'s H6 entry and `08`'s
D-54..D-59.

**The shape.** The harness owns sub-agents; the adapter only maps them onto
what already exists for grok's children. `internal/harness/tool/opencode/agent.go`
prepares each call and hands it to a new interface, `Env.Subagents`;
`internal/harness/subagents.go` (the runner) resolves the persona, model and
effort, takes a slot under the cap, opens a child `harness.Session`
(`harness.Open(childOptions)`), runs it, closes it, and turns its events into
three harness events (`SubagentStarted`, `SubagentEvent`, `SubagentFinished`)
that the adapter maps onto `EventSubagent`, `Event.Agent`-tagged child
events, the roster, and the task row — **no new `agent.Event` kind or field**
for foreground children. **A child is a second in-process `harness.Session`,
not a child process** (D-54, superseding D-10): the same shape grok-build,
opencode, codex and crush all use.

**The `agent` tool.** Id `agent` ("Claude Code calls this tool `Agent`
(formerly `Task`); grok calls it `task`" — a deviation from D-53's
grok-build-id pattern, since "task" means the todo panel in craze's TUI).
Kind **`task`**, a new `tool.Kind` (see the tool table above; this
correction is D-55). Parameters: required `description` (3–5 words) and
`prompt`; optional `subagent_type` (default `general-purpose`), `model`,
`effort`, and `run_in_background` (boolean, PR 3): a call that sets it true
runs in the background on a session that supports it (native, interactive)
and in the foreground otherwise — tolerated as a no-op from PR 1, it now
takes a slot the same way, fails fast rather than waiting when the four are
all held, and returns its acknowledgement at once with no usage. The
description is a static `agent.txt`
plus a per-session tail — one row per agent type (8 KiB) and one row per
model plus the tier map (2 KiB), each folded to one line and redacted —
rendered by a decorator over the static spec, so `specs.golden` still pins
the static text. `ModeGate` **allows `agent` by name ahead of the mode
switch** (the reverse of `exit_plan_mode`'s deny-by-name): the call itself
always reaches the runner, and the child's own mode does the restricting.

**Personas.** See `06`'s Personas (H6) section for the three sources, the
built-ins, and precedence. The harness receives them as data
(`harness.Options.Personas`) and never reads a file itself.

**Mode and tools.** A child inherits the parent's mode, fixed at `Open`, and
never gets `agent`, `todo_write`, `ask_user_question`, or `exit_plan_mode`.
A later parent mode switch **tightens** a running child, never loosens it,
through a monotonic strictness pushed under the runner's registry lock;
agent → plan gives the child no plan file (the empty-plan-path rule then
denies every edit); agent → ask makes it read-only. `Locks` is the parent's
`PathLocks`, shared, so a parent's and a child's edits of one file still
serialize.

**Model and effort.** `models.toml` gains an optional `[subagents]` section:
a default model, a default effort, and a `[subagents.tiers]` map from a
Claude-style tier name (`opus`, `sonnet`, `haiku`) to a native alias.
Resolution, first hit wins: the call's value, the persona's, the configured
default, then the parent's running-turn model/effort. A tier name maps
through the table; an unmapped tier falls back to the parent's model, and
`inherit` always means the parent's. An unknown **call** value is an error
listing the valid aliases and tiers; an unknown persona or default value
falls through with a diagnostic, since content does not get to force a
resolution failure.

**Results.** The tool result follows a fixed table keyed on how the child's
turn ended — clean, no text, cut off, refusal, doom loop, step limit,
failed, the parent's own turn cancelled, or stopped by the user (Plan 026
§3.7 has the exact texts). Usage is **observed, not returned**: the runner
sums the child's per-step usage from its own sink and stamps it onto the
parent's tool entry as `subagent_usage`, priced per model (`03`). One shared
truncation cap (2000 lines / 50 KiB, with a spill file) covers both a
successful and a failed result.

**The runner.** A leaf-locked registry of live children; a semaphore of
**4** children per session at once (foreground and background together),
sitting above Fantasy's own 5-parallel-tool cap; several `agent` calls in
one step run concurrently, and a 5th waits for a slot. A call returns only
after its child's `Run` and `Close` both return, so nothing of a child
outlives the call. `Close` signals every child's closing before joining
them, ahead of an ordinary cancel, so a child's `bash` gets no grace once
the session is really closing. Depth is 1: a child has no `agent` tool, and
the runner refuses when its own session is itself a child.

**Events.** The runner forwards each child event straight to the parent's
sink, never taking the parent turn's own mutex, since a child's callbacks
run under its own turn's lock. The adapter maps them onto the roster
(`EventSubagent{spawned|progress|finished}`), an `EventUser` that opens the
child's transcript with its prompt, and the existing task row
(`ToolEvent.Task`, `Task.Receipt = true` once the row is final). Ordering is
pinned: `SubagentStarted` before every event of that child, every event
before `SubagentFinished`, and `SubagentFinished` before the parent's own
`ToolFinished` for the call — enforced by enqueuing roster changes under the
adapter's roster lock and flushing after `spawned` and after `finished`. The
32-finished roster eviction rule is unchanged from grok's.

**Per-child cancel (PR 2).** `(*Session).CancelSubagent(id)` cancels one
child's context; `SubagentCanceller` is an optional interface found by type
assertion, as `EventSource` is, so `agent.Session` does not change.
`engine.Control` gains one non-waiting method. Backspace/Delete on a
focused running child row, or inside its view, stops just that child —
gated on a new `SubagentCancel` capability, so the key falls through to the
composer everywhere else, grok included.

**Background children (PR 3, shipped).** `run_in_background: true`, off by
default (`Options.Background`), on only for an interactive session —
headless `craze prompt` always runs a background call in the foreground. A
background child runs on a session-level worker outside the turn, taking
one of the same four slots by occupancy: it never waits, and fails fast
when none is free, where a foreground call still waits behind another
foreground holder (`busyText`: "All 4 sub-agent slots are in use; wait for
one with agent_output or stop one."). A turn cancel (Esc) spares it; only a
per-child stop or `Close` ends it. Its result is redacted and capped like a
foreground answer, then reserved and committed at the next step boundary,
delivered by the new `agent_output {id, wait_ms}` tool (read-only,
parallel, `wait_ms` default 30000 ms, capped at 600000, 0 does not wait; a
result already delivered, or already part of the model's own input, says so
instead of repeating it), or delivered by the session's own **wake** —
never through the steer box or `Result.Unanswered`, so a background result
can never be requeued as a user prompt. A result that finishes while the
session is idle wakes the parent once: the session starts a turn of its own
bracketed by `EventForeignTurn`, exactly as a live session's foreign turns
are, and fenced by the new `agent.AdmissionFence` so the engine never
admits a craze prompt while the wake runs; the note's wording is carried on
the new `ForeignTurnInfo.Reason` field ("subagent_wake") — the one new
`agent.Event` field this plan adds. The row band marks a background child
`bg`, gated by `Capabilities.SubagentBackground` (native only; grok's own
background flag does not move its goldens). A drained row refused by the
wake is restored at the queue's head (SF-21, widened to every row-sourced
turn a foreign turn refuses).
