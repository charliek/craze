# 02 — Architecture

## The seam

craze's TUI never imports `internal/acp`. Everything flows through
`agent.Session` (`internal/agent/session.go`), the `Snapshot` and `Event`
types, and `agent.New(agent.Options{Provider: &p})`, which the three CLI
entry points (`internal/cli/tui.go`, `prompt.go`, `frame.go`) all call via
`tui.Config.NewSession`. The native harness is a second implementation of
that interface. The TUI, the queue, the cards, `craze prompt --json`, and the
frame runner do not change.

```mermaid
flowchart LR
  tui["TUI / prompt --json"] --> session["agent.Session"]
  session --> live["live (ACP) session"]
  session --> native["native session (adapter)"]
  live --> acp["acp.Client → cursor / grok / gx"]
  native --> harness["internal/harness"]
  harness --> providers["catalog replacement + overlay → provider factory"]
  providers --> fantasy["fantasy LanguageModel.Stream"]
  harness --> store["JSONL tree store"]
  harness --> tools["tool stack"]
```

H0 accepted Fantasy `v0.43.2` only at its public provider and
`LanguageModel.Stream` boundary. The released `Agent.Stream` loop is not a
production dependency: it deterministically stops instead of dispatching a
complete tool call when the normalized finish reason is `stop`. The harness
therefore owns step continuation, message history, usage and finish
accounting, cancellation, and callback-to-event mapping. H0 also rejected
Catwalk `v0.52.43` as the production catalog, so H1 is blocked until a
separate panel-reviewed spike selects a replacement.

## Package split

| package | owns | must not import |
|---|---|---|
| `internal/harness` | catalog adapter, Fantasy provider factory, direct-stream turn runner, tools, permissions, store, compaction, prompt assembly, import command logic | `internal/agent`, `internal/tui`, `internal/acp` |
| `internal/agent` (new file, e.g. `native.go`) | the adapter: `Provider` constructor for `native`, `Session` implementation that maps harness callbacks onto `Event`/`Snapshot` | — |
| `internal/cli` | flag plumbing only (`--provider native`), the `import` subcommand entry | — |

The rule that matters: **`internal/harness` knows nothing about craze
types.** It exposes its own small event and request types. That is what
keeps a later ACP server binary a wrapper rather than a rewrite, and what
lets the harness be tested with a scripted Fantasy `LanguageModel` instead
of a fake agent process.

## The turn loop

The loop follows the common shape observed in opencode, grok, and pi, but is
implemented by craze over Fantasy's public direct-stream API:

1. **Re-read history from the store every iteration.** No in-memory
   conversation across steps. Queued messages, interject, resume, and
   compaction all fall out of this.
2. **Build the prompt**: system prompt (assembled once per session, see
   `06`), instruction files, skill catalog, then the message list from the
   store's leaf-to-root walk with the latest compaction applied.
3. **Drain steering** (interject) into the message list at the safe boundary
   before the next stream request. History replacement is owned by craze;
   it is not delegated to `Agent.PrepareStep`.
4. **Compact if needed** before sampling (`03`, `07`).
5. **Call `LanguageModel.Stream`**, collect normalized text, reasoning,
   tool-input, finish, usage, warning, and error parts, and map them onto the
   harness's events. A stream is complete only after an explicit finish
   part; an in-band error without one is a failed step.
6. **Persist the assistant step and tool results, then continue based on
   complete parts, not only finish reason.** A complete, schema-valid tool
   call continues even when the provider reports `stop`. Manual history must
   retain adjacent assistant-call/tool-result pairs in order.
7. **Guard execution**: three consecutive identical tool calls (name +
   input) raise a doom-loop permission ask; a `length` stop with tool calls
   fails the calls rather than executing possibly truncated arguments (D-07).
8. **Cancel** through context cancellation. In-flight tool calls are closed
   as errored results marked interrupted so every tool call has a result on
   replay. If the turn produced no output, the prompt is put back on the
   queue (grok's rewind).
9. **Drain follow-ups** after the turn through craze's existing queue. The
   harness does not own the TUI queue.

This owned loop adds roughly 1.5–2.5 kLOC plus a comparable body of fixtures
before the later tools and permissions phases. That is an H0 sizing estimate,
not an implementation commitment; H1 must refine it in its plan.

## Tool stack

Every tool is exposed through Fantasy's public tool schema but dispatched by
craze, wrapped in this order, innermost first:

1. the tool itself (read, bash, edit, …)
2. **kind metadata** — attaches grok's `x.ai/tool` vocabulary (kind,
   namespace, read_only, canonical input) so craze's cards key on kind
   unchanged (`05`)
3. **truncation** — full output to disk, head/tail preview plus path
4. **permission** — asks through the session, honours grants, removed
   entirely under `Force` (yolo)
5. (later) hooks, MCP — same wrapper shape, not built now

Modes filter which tools are active and add a prompt reminder; plan mode is
additionally enforced in the dispatcher so it survives yolo (`05`).

## Home directory

Default `~/.craze-acp/`, overridden by one environment variable
(`CRAZE_ACP_HOME`; name is D-05). Layout:

```
~/.craze-acp/
  config.toml          provider overlay, aliases, compat toggles
  instructions/        imported global CLAUDE.md / AGENTS.md
  skills/              imported user skills
  agents/              imported agent definitions
  plugins/<id>/        imported plugin snapshots + manifest.json (source, sha)
  sessions/<cwd>/<id>.jsonl
  permissions/<cwd>.toml
```

craze's own TUI config stays at `~/.craze/config.toml` (`CRAZE_CONFIG`)
unless `10-open-questions.md` Q1 is resolved otherwise.

## Testing pattern

- **Unit**: a scripted Fantasy `LanguageModel` returns canned text,
  reasoning, tool calls, finish reasons, usage, warnings, and in-band errors.
- **Wire**: local canned server-sent events exercise Fantasy's released
  provider normalization; a small reviewed cassette set may be added only
  where a real provider shape cannot be represented locally.
- **Golden**: the existing TUI frame and golden tests run against the native
  session with the scripted model.
- **Live**: each phase ends with a tmux smoke on the mac-mini against real
  providers (see the `craze-tmux-live-smoke` memory for the procedure).

## Capabilities

The adapter fills `agent.Capabilities` per phase so the TUI hides what the
harness cannot do yet instead of drawing empty surfaces:

| capability | phase it turns on |
|---|---|
| Effort, Modes (model/effort options) | H1 |
| Interject | H1 |
| Todos, AskCards, PlanCards | H5 |
| SubagentRows, SubagentTranscript | H6 |
| FastToggle | never (cursor-only) |
