# 05 — Tools and permissions

## Tool set

Ship in this order (D-06). pi's default loadout is the first four.

| tool | kind | permission | rules |
|---|---|---|---|
| `read` | read | none | 2000 lines / 50 KiB cap, `offset`/`limit`, line-number prefix, "continue with offset" hint; images returned as attachments when the model accepts them; binary detection; directory listing mode |
| `bash` | execute | ask | user's shell; default timeout 120 s, absolute cap 10 min; kill process group on timeout/cancel; streamed live output to the tool card; tail-truncated to 2000 lines / 50 KiB with the full output saved to a file whose path is reported; non-zero exit is an error result. No background jobs (use tmux; grok's auto-background is deferred) |
| `edit` | edit | ask | `{path, edits: [{old, new}]}` multi-edit in one call (pi); replacer ladder: exact, trimmed, block-anchor, whitespace-normalized, indentation-flexible, escape-normalized (opencode), each requiring a unique match unless `replaceAll`; refuse disproportionate matches; preserve BOM/CRLF; per-file mutation queue because tools run in parallel; returns a unified diff the card renders through `internal/textdiff` |
| `write` | edit | ask | create or overwrite, mkdir -p; diff against the previous content |
| `ls` | read | none | 500 entries |
| `glob` | search | none | 1000 results, sorted by mtime |
| `grep` | search | none | ripgrep JSON, 100 matches, 500 chars per line |
| `todo_write` | other | none | H5; replaces the list; feeds `EventTodos` |
| `ask_user_question` | other | blocks | H5; feeds `EventQuestion`; 30-minute timeout returns "unanswered" |
| `exit_plan_mode` | other | blocks | H5; reads the plan from the plan file, never from arguments; outcome approved / cancelled / abandoned, unknown → cancelled |
| `agent` | think | derived | H6; see below |

Rules that apply to every tool. (Paths under `~/.craze/native/` in this
document are provisional: each phase designs its own files, and all of them
move with `CRAZE_HOME` — D-27, `02`.)

- **No read-before-edit enforcement.** opencode deleted theirs; grok and pi
  never had one. Prompt-only.
- **Truncation is one wrapper**, not per tool. Full output always lands in
  `~/.craze/native/tool-output/<call-id>` with a 7-day sweep.
- **Parallel by default** (fantasy `NewParallelAgentTool`); a tool can
  declare itself sequential and force the batch sequential.
- **Tool ids are stable.** Permission keys, grants, and the kind table
  reference them; renaming a tool is a migration.
- **Descriptions are templates** rendered once per session so they can
  reference each other and the configured limits.

## Kind metadata

Each tool call carries the vocabulary craze's cards already read from grok:
`kind` (read, edit, execute, search, fetch, think, other), `namespace`
(`native`), `read_only`, and a canonical input projection (`path`,
`command`, `pattern`, `cwd`, …); bulky edit bodies stay in `RawInput`.
`ToolEvent.Kind`, `Title`, `Locations`, `Output`, and `Diffs` are filled
from this in the adapter.

## Permission model

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
  `Read`→`read`, `Edit`/`MultiEdit`/`Write`→`edit`/`write`, `Glob`→`glob`,
  `Grep`→`grep`, `Agent`/`Task`→`agent`.
- `Force` (yolo) removes the permission wrapper entirely; the dangerous
  list still applies only through explicit deny rules.
- Doom loop: three consecutive identical calls raise a `doom_loop` ask.

## Modes

Modes are **rulesets plus prompt reminders**, never separate loops.

| mode | tools | extra |
|---|---|---|
| implement | all | — |
| plan | read, ls, glob, grep, bash; `edit`/`write` only to the plan file; no `agent` | plan reminder appended as a synthetic user part; `exit_plan_mode` available |
| ask | read-only set | ask reminder |

Plan mode is enforced in the **dispatcher** (any edit-kind call outside the
plan file is rejected with a model-facing message), so it survives yolo.
The plan file is `~/.craze/native/sessions/<cwd-slug>/<id>.plan.md`. Leaving plan
mode by accepting the plan sends the provider's implement prompt exactly as
craze does for cursor and grok.

## Sub-agents (H6)

- `agent` tool: `{prompt, description, subagent_type?}`; depth 1 by default;
  concurrency cap 4, queued beyond that.
- **First cut spawns a child craze process** (`craze prompt --json` with
  the native provider) and parses its event stream (D-10). Isolation and
  the headless path come free; in-process nesting via fantasy is the
  follow-up if latency or shared state demands it.
- Child permissions = parent's denies + no `agent`, no `todo_write`.
- Child events arrive tagged with the child id; craze's `SubagentInfo`
  rows and per-child transcript already exist.
- Result is the child's last text, capped at 50 KiB; child usage folds into
  the parent; parent cancel propagates.
- Personas come from workspace `.claude/agents/*.md`, imported
  `~/.craze/native/agents/`, and plugin `agents/` (`06`).
