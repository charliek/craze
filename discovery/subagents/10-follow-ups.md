# 10 — Follow-ups

`TF-nn`. Anything found and not done, by the phase that should take it. A
plan's work breakdown says which rows it takes; a row it closes is deleted.

| ID | Found | What | Phase |
|---|---|---|---|
| TF-1 | 2026-10-10 | `cc-plugins`' rescue agents (`cursor-rescue`, `grok-rescue`, `codex-rescue`) have no `model` field, so each forwarding call runs on the parent's model, usually Opus. Add `model: haiku`. Owner's call; outside this repo. | any |
| TF-2 | 2026-10-10 | Flag inconsistencies G5–G8 ([05](05-cli-gaps.md)): `new` lacks `--force`; `-C`/`--dir` against `--workspace`; `--json`'s four meanings; `--model`'s help text. | T2 |
| TF-3 | 2026-10-10 | G3 and G4 on the hub path: `session.create` cannot start a session in plan or ask mode, mark it unattended with its start settings, pass plugin directories, or continue a session. T1's waiting commands avoid the hub (TD-08); plain `craze new` and other hub clients (shed) still need these create params. | T2 |
| TF-4 | 2026-10-10 | Compare `codex exec` and craze native on the same model with `eval/`, over enough samples to decide TQ-3. | T2 |
| TF-5 | 2026-10-10 | A sandboxed read-only mode for native: shell allowed, writes blocked by the OS rather than by the mode gate, which cannot see inside a shell (`modegate.go:135-138`). Needed before craze can replace `codex exec` for reviews. | T2 |
| TF-6 | 2026-10-10 | An MCP server (`craze mcp`) exposing the same operations as tools, if the wrapper's overhead or Bash's constraints bite (TD-07). | T3 |
| TF-7 | 2026-10-10 | G2: `craze prompt` cannot continue or load a session. | T2 |
| TF-8 | 2026-10-10 | `craze prompt --json` never prints usage, because a usage-only delta has no JSON line (`docs/reference/protocol.md:1331`). Put the session's usage on the `done` line. Deferred from T1: it would move the V2 parity check. | T2 |
| TF-9 | 2026-10-10 | A shorter idle exit for unattended hosts (TQ-8): today one stays until `craze stop`. `craze send` reloads an ended session now (TD-12), so ten minutes or so is safe. | T2 |
| TF-10 | 2026-10-11 | `craze prompt --plan` on grok: an accepted plan lets grok leave plan mode and edit in the same turn (live L7 on the unattended host). `craze prompt` still accepts plans (its V2 parity check); give it TD-11's return policy. | T2 |
| TF-11 | 2026-10-11 | A plan-mode sub-agent stays in plan mode: `craze send` takes no mode, and a reload restores the recorded `plan`. A mode flag on `send` (or a mode command) would let a follow-up implement the returned plan; today it is a new delegation. | T2 |
| TF-12 | 2026-10-11 | The waiter's result accumulator mirrors the transcript's split rules (`internal/cli/turnresult.go`); folding through the transcript model would remove the duplication. | T2 |
| TF-13 | 2026-10-11 | Under a stalled encoder past the handler's bound, the return policy's cancelled plan reply can reach the agent before its `session/cancel` (TD-11). It accepts nothing, and the next prompt stays fenced; seen in review only. | any |
| TF-14 | 2026-10-11 | A direct-spawned host inherits the command's working directory (the hub sets the workspace). A host whose directory is later removed, such as a deleted worktree, keeps a dead cwd. Set it to the workspace. | T2 |
| TF-15 | 2026-10-11 | Haiku's "random" prompt-file names repeat across runs. `Write` refusing an existing unread file catches a collision, and the wrapper picks another name; a craze-side default name would drop the step. | any |
| TF-16 | 2026-10-11 | A shared reload whose intermediate stalls between starting the host and exiting leaves the host in the caller's process tree for that time, so a TaskStop then could reach a host another `send` is using (branch review, accepted). Normally the intermediate exits within milliseconds, before the host can register. Blocking the stop signals in the host until it is reparented would close it. | any |
