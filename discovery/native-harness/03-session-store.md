# 03 — Session store

Adopted from pi's format (D-03), which is the simplest of the three
reference designs and gives branching, fork, and rewind for free.

## One file per session

```
~/.craze-acp/sessions/<cwd-encoded>/<unix-ms>_<uuid>.jsonl
```

`<cwd-encoded>` is the absolute workspace path with `/`, `\`, and `:`
replaced by `-`, leading separator dropped (pi's scheme). Sessions are
workspace-scoped like grok's; `--resume` lists the workspace's directory.

## Entries

Line 1 is the header. Every other line has `id` (short random), `parentId`,
`ts` (unix ms), and `type`. Entries form a **tree**; the session keeps a
**leaf** pointer. Context is rebuilt by walking leaf → root.

| type | payload | in model context |
|---|---|---|
| `session` | `version`, `id`, `cwd`, `parentSession?`, `name?` | header |
| `message` | one message: `user`, `assistant`, `tool` (result), `system` | yes |
| `model_change` | `provider`, `model` | no (affects later requests) |
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

## Rules

- **Append-only.** Nothing is rewritten. A rewind moves the leaf; a new
  message after a rewind starts a branch. Abandoned branches stay in the
  file (optionally summarized into a `branch_summary`).
- **Fork** copies the file to a new id with `parentSession` set.
- **Reasoning** is stored as its own part in the assistant message and is
  dropped from the outgoing request when the model differs from the one
  that produced it (`04`).
- **Interrupted tool calls** are closed with an errored tool result marked
  `interrupted` before the next entry is written.
- **Compaction** never deletes; it appends a `compaction` entry and, like
  grok, writes the discarded segment to
  `sessions/<cwd>/<id>.compaction/segment_NNN.md` with an index the summary
  points at.
- **Replay for the TUI** is the same leaf→root walk emitting `Event`s in
  order; `--continue` and `--resume` (plan 013's surfaces) are redone on it.
- **Torn last line** (crash mid-write) is tolerated on load.

## Cost and usage

Each assistant message carries usage; the session total is the sum over the
current branch. Prices come from the catalog (`04`); the status row shows the
turn and session cost from H7.
