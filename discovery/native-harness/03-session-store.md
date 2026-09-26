# 03 — Session store

Adopted from pi's format (D-03), which is the simplest of the three
reference designs and gives branching, fork, and rewind for free.

## One file per session

```
~/.craze/native/sessions/<cwd-slug>/<UTC yyyymmddThhmmssZ>_<session-id>.jsonl
```

(Path updated per D-27; the timestamp prefix is a UTC `yyyymmddThhmmssZ`
stamp rather than unix milliseconds.) `<cwd-slug>` is the absolute
workspace path with the leading separator stripped, `/`, `\`, and `:`
replaced by `-`, and the result wrapped in `--…--` (pi's scheme). Sessions are workspace-scoped like grok's;
`--resume` lists the workspace's directory.

**Plan 018 §3.6 narrows this for H1**: the store ships with a header line
plus `message`, `model_change`, and `effort_change` entry types only (no
`compaction`, `branch_summary`, `label`, `session_info`, or `custom` yet),
the file is created lazily on the first turn, and native sessions stay out
of the shared index (`sessions.jsonl`) until H7. Where the rest of this
document describes a richer entry set or eager creation, H1 does not
implement it yet. H1's entry fields are also what this document's table
below should be read through: every entry carries `timestamp` (not `ts`);
a `message` entry is Fantasy's official message JSON inside craze's
envelope with `provider`, `model`, `wire_model` (as they were when the entry
was written), and optional `effort`, `usage`, `stopReason`, `interrupted`
(D-33); `model_change` carries `provider`, `model`, and `wire_model`.

## Entries

Line 1 is the header. Every other line has `id` (8 hex chars), `parentId`,
`timestamp`, and `type`. Entries form a **tree**; the session keeps a
**leaf** pointer. Context is rebuilt by walking leaf → root.

| type | payload | in model context |
|---|---|---|
| `session` | `version`, `id`, `cwd`, `parentSession?`, `name?` | header |
| `message` | one message: `user`, `assistant`, `tool` (result), `system` | yes |
| `model_change` | `provider`, `model`, `wire_model` | no (affects later requests) |
| `effort_change` | `effort` | no |
| `mode_change` | `mode` | no |
| `compaction` | `summary`, `firstKeptId`, `tokensBefore`, `usage` | replaces everything before `firstKeptId` |
| `branch_summary` | `fromId`, `summary` | yes, as a user-visible note |
| `label` | `text` | no (rewind targets) |
| `session_info` | `name` | no |
| `custom` | free-form, `inContext: bool` | per entry |

Message payloads are fantasy's message shape serialized with stable field
order and `omitempty` everywhere (byte-stable JSONL keeps provider prompt
caches warm, grok's lesson).

**Plan 028 (H7, planned) adds new fields and two new entry types**, layered
on H1's narrowing above without changing it. A user `message` entry gains
`turn: N` (the turn's number, written on the user entry a `Run`/`Wake` opens
turn N with — never on a steer or on mid-turn results): turn boundaries on
replay, turn numbering, and the last turn's spend all read it (H7 PR 1). A
tool `message` entry gains `todos: [...]` (the session's todo list after a
step whose calls changed it), so resume restores the list the model last
built instead of re-applying `todo_write` calls in transcript order, which
parallel or refused calls make unsound (H7 PR 1). A `resume` entry, held
like a change and written with the first step after a resume, carries
`system_prompt_sha256`, `tools_sha256`, `tool_profile`, `craze_version` —
each incarnation's contract, as information (H7 PR 1). A `reminder` entry,
spliced into a step's batch at its request position, carries only
`variant` — the rendered text is never stored (Plan 023 owner decision 5);
`variant` enumerates every distinct rendered text a mode reminder can take,
so re-rendering from `(variant, plan path)` is byte-identical without
reading the plan file (H7 PR 2, D-66). `compaction`'s payload grows to
`turn` (the turn it ran in, or its own turn for a manual compaction),
`summary` (omitempty), `firstKeptId` (omitempty = no tail), `tokensBefore`,
`tokensAfter`, `reason` (`auto`\|`manual`\|`overflow`), `command` (the typed
`/compact …`, manual only), `provider`, `model`, `wire_model`, `usage` (every
attempt, summed), `segment` (file name, omitempty), `error` (omitempty) —
exactly one `compaction` entry per compaction: a summary, or an `error` and
no summary, never both (H7 PR 2). An older craze ignores the new keys and
keeps `resume` and `reminder` entries in the tree without ever reaching
context, as it already does for any unknown type.

## Rules

- **Append-only.** Nothing is rewritten. A rewind moves the leaf; a new
  message after a rewind starts a branch. Abandoned branches stay in the
  file (optionally summarized into a `branch_summary`).
- **Fork** copies the file to a new id with `parentSession` set.
- **Reasoning** is stored as its own part in the assistant message and is
  dropped from the outgoing request when the entry's `provider` **or**
  `wire_model` differs from the current model's (D-33); the file itself is
  never rewritten.
- **Interrupted tool calls** are closed with an errored tool result marked
  `interrupted` before the next entry is written.
- **Compaction** never deletes; it appends a `compaction` entry and, like
  grok, writes the discarded segment to a markdown file the summary points
  at. **Plan 028 (H7 PR 2, planned) amends the path to
  `<stem>.compaction/segment_NNN.md` beside the transcript** (`<stem>` = the
  transcript's name without `.jsonl`, as `PlanPath` does), not
  `<id>.compaction/` (D-63): craze's sessions are files, not directories
  like grok-build's, so the folder sits beside the transcript the way the
  plan file already does. `NNN` is the number of successful compactions on
  the path plus one; the segment is written to a temp file then renamed
  onto `segment_NNN.md` under the store's lock, before its `compaction`
  entry, so an entry never names a missing file and a crash between the two
  simply leaves an orphan the next compaction replaces.
- **The context rule after a compaction** (H7 PR 2, planned; §3.9): reading
  forward from the latest **successful** compaction `C` on the path (a
  failed `compaction` entry — `error`, no `summary` — is skipped, as if it
  were never there), context is the summary message, then the entries from
  `C.firstKeptId` up to `C` (the kept tail, when there is one), then
  everything after `C` — each part filtered by today's rules (D-33's
  reasoning drop, the empty-assistant drop). The trailing-unanswered trim
  applies to entry messages only and never removes the summary message, so
  a context may legally end in it (the provider then sees a bare user
  message, which is accepted like any other empty-prompt continuation).
- **Replay for the TUI** is the same leaf→root walk emitting `Event`s in
  order; `--continue` and `--resume` (plan 013's surfaces) are redone on it.
- **Torn last line** (crash mid-write) is tolerated on load. **Plan 028 (H7
  PR 1, planned) turns this into a repair on open, not just tolerance**:
  `Open`ing a stored session takes an exclusive `flock` on the file, held
  until `Close`; a torn last line or a cut step's dropped bytes are written
  to a sibling `.torn-<UTC-stamp>` backup (fsynced, its directory fsynced
  too, so the backup's own name is durable) and only then truncated away —
  never rewritten in place, never destroyed. A dropped entry of a type this
  craze does not know refuses the trim instead (`ErrNewerTranscript`): never
  destroy what a newer craze wrote. `New`'s hard-link publish takes the same
  lock first, on the temp file's descriptor, so a resume and a fresh write
  can never race the same session id (D-60).

## Sub-agents (H6)

**Status: planned (Plan 026), not built.** A child's transcript is an
ordinary session file, in the same `sessions/<cwd-slug>/` directory as any
other. Its header gains four additive keys beside the usual `session`
fields: `parent_session`, `parent_tool_call` (the parent's `ToolStarted`
id), `subagent_type` (the resolved persona name), and `persona_path` (the
source file, when the persona came from one, as provenance only — personas
never enter the parent's skills catalog). An old header loaded without them
still loads (lenient decode). `sessions.jsonl`, the shared index, is
untouched — native sessions stay out of it until H7, and a child is no
exception.

A tool-call `message` entry gains an additive field, `subagent_usage`: a
list of `{provider, model, wire_model, usage}` rows, one per model a child
ran on, merged onto the entry that owns the call (the `stepFinished` and
`synthesizeStep` paths both write it). A tool entry never carries a plain
`usage` — only `subagent_usage` — because the entry is stamped with the
**parent's** model, and a plain `usage` there would price a child that ran
on another model at the parent's rate.

## Cost and usage

Each assistant message carries usage; the session total is the sum over the
current branch. Prices come from the catalog (`04`); the status row shows the
turn and session cost from H7.

**The one accounting rule (H6, pinned):** a session's cost is the sum of the
`usage` on its own assistant entries, each priced by that entry's model,
**plus** the `subagent_usage` rows on every one of its own entries that owns
them — whatever the entry's role, tool or user — each priced by the row's
own model. A child's transcript is a record, never added into a parent's
cost, so nothing is counted twice. H7's cost row follows this rule.

**The accounting rule, extended (H7, planned; Plan 028 §3.14, PD18):** a
session's spend is the H6 rule above **plus every `compaction` entry's
`usage`** (success or failure; every attempt summed) **plus this
incarnation's observed usage that no entry holds** (a step whose append
failed; compaction attempts whose entry could not be written) — journaled,
never a second copy of what an entry already carries. A child's transcript
is still never added in; a child emits no `Spent` of its own, and its
`SubagentUndelivered` usage stays journal-only, as it already does (H6).
**A turn's spend** is its steps' usage plus its compactions' usage plus the
`subagent_usage` its entries carry, where an entry belongs to the turn of
the latest `turn`-marked record at or before it on the path, and a
compaction entry to its own `turn` — so a pre-turn compaction counts toward
the turn it precedes, and a manual one is a turn of its own. After a resume,
turn numbering continues from the largest `turn` recorded on the path, so a
turn that persisted nothing, or a manual compaction, leaves no gap to reuse.
Money is integer picodollars (D-64): each price is rounded once at load, so
every sum stays exact.
