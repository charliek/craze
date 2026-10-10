# 10 — Follow-ups

`TF-nn`. Anything found and not done, by the phase that should take it. A
plan's work breakdown says which rows it takes; a row it closes is deleted.

| ID | Found | What | Phase |
|---|---|---|---|
| TF-1 | 2026-10-10 | `cc-plugins`' rescue agents (`cursor-rescue`, `grok-rescue`, `codex-rescue`) have no `model` field, so each forwarding call runs on the parent's model, usually Opus. Add `model: haiku`. Owner's call; outside this repo. | any |
| TF-2 | 2026-10-10 | Flag inconsistencies G5–G8 ([05](05-cli-gaps.md)): `new` lacks `--force`; `-C`/`--dir` against `--workspace`; `--json`'s four meanings; `--model`'s help text. | T2 |
| TF-3 | 2026-10-10 | G4: `session.create` cannot pass plugin directories or continue a session. | T2 |
| TF-4 | 2026-10-10 | Compare `codex exec` and craze native on the same model with `eval/`, over enough samples to decide TQ-3. | T2 |
| TF-5 | 2026-10-10 | A sandboxed read-only mode for native: shell allowed, writes blocked by the OS rather than by the mode gate, which cannot see inside a shell (`modegate.go:135-138`). Needed before craze can replace `codex exec` for reviews. | T2 |
| TF-6 | 2026-10-10 | An MCP server (`craze mcp`) exposing the same operations as tools, if the wrapper's overhead or Bash's constraints bite (TD-07). | T3 |
| TF-7 | 2026-10-10 | G2: `craze prompt` cannot continue or load a session. | T2 |
| TF-8 | 2026-10-10 | `craze prompt --json` never prints usage, because a usage-only delta has no JSON line (`docs/reference/protocol.md:1331`). Put the session's usage on the `done` line. Deferred from T1: it would move the V2 parity check. | T2 |
| TF-9 | 2026-10-10 | A shorter idle exit for unattended hosts (TQ-8), once `craze send` can reload an ended session. | T2 |
