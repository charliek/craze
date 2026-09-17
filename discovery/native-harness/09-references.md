# 09 — References

All clones live under `../thirdparty/` (sibling of this repo). Dates are
the commit inspected. The four review reports that produced `05`–`08` are
summarized in the owner's memory notes (`fantasy-harness-discovery`,
`harness-reference-review`); this file is the map.

## Libraries we build on

| repo | version / date | license | why | read first |
|---|---|---|---|---|
| `fantasy` (`charm.land/fantasy`) | v0.43.2, 2026-09-15 | Apache-2.0 | the model layer and agent loop | `agent.go` (`AgentStreamCall` callbacks, `PrepareStep`, `StopWhen`), `tool.go`, `content.go`, `providers/openaicompat/`, `providers/openrouter/` |
| `catwalk` (`charm.land/catwalk`) | 2026-09-15 | MIT | provider/model catalog with prices and flags | `pkg/catwalk/provider.go`, `pkg/embedded/`, `internal/providers/configs/{fireworks,zai,openrouter,xai,openai}.json` |
| `acp-go-sdk` (`github.com/coder/acp-go-sdk`) | v0.13.5 | — | the ACP server wrapper, later | `README.md`, `types_gen.go` (`Agent` interface), `extensions.go`, `example/agent/main.go` |

Go requirements: fantasy 1.27, catwalk 1.26; craze pins 1.24 in `.mise.toml`.

## Harnesses we learned from

| repo | date | license | what it grounded |
|---|---|---|---|
| `crush` | 2026-09-11 | **FSL-1.1** (port ideas, do not copy code) | how a coding agent sits on fantasy: `internal/agent/agent.go` (callbacks → messages, `PrepareStep` queue fold), `coordinator.go` (provider factory by catwalk type, `buildTools`), `hooked_tool.go` (the wrapper pattern), `agent_tool.go` (sub-agent as a parallel tool), `internal/permission/` |
| `opencode` | 2026-08-30 | MIT | the turn loop (`session/prompt.ts`, `processor.ts`), permissions (`permission/index.ts`, `arity.ts`), tools (`tool/edit.ts` ladder, `tool/truncate.ts`, `tool/shell.ts`), compaction (`session/compaction.ts`), instruction files (`session/instruction.ts`), provider quirks (`provider/transform.ts`), ACP server (`acp/`) |
| `grok-build` (xAI, plus the gx fork in `../grok-build`) | 2026-09-09 | — | the reference for craze's current wire; Claude compat (`xai-grok-agent/src/prompt/agents_md.rs`, `discovery.rs`, `plugins/discovery.rs`, `xai-grok-tools/.../skills/`, `xai-grok-workspace/src/permission/claude_settings.rs`, `xai-grok-shell/src/claude_import.rs`), tool taxonomy (`xai-grok-tools/src/tool_taxonomy.rs`, `normalization.rs`), plan mode (`session/acp_session_impl/tool_calls.rs`), sessions (`storage/`, `updates.jsonl`), compaction (`session/compaction.rs`, `xai-compaction-transcript`), sub-agents, the user guide under `xai-grok-pager/docs/user-guide/` (08 skills, 09 plugins, 12 project rules, 19 plan mode, 22 permissions) |
| `pi` (`earendil-works/pi`) | 2026-09-16 | — | the minimal core: `packages/agent/src/agent-loop.ts`, `agent.ts`; `packages/coding-agent/src/core/session-manager.ts`, `core/tools/`, `core/compaction/`, `core/extensions/`, `core/package-manager.ts`; docs `session-format.md`, `extensions.md`, `skills.md`, `packages.md`, `compaction.md`, README "Philosophy" |

## Sizes that shaped the estimates

| codebase | subsystem | lines |
|---|---|---|
| pi | loop + steering | 2.5k TS |
| pi | tools (8) | 3.4k |
| pi | session manager | 1.8k |
| pi | compaction | 1.6k |
| opencode | session core (loop, processor, compaction, tools glue) | 8.1k TS |
| opencode | tools | 5.5k |
| opencode | permissions | 0.4k |
| opencode | provider quirks | 4.4k |
| grok | tools crate | 150k Rust (60k in the grok_build set) |
| grok | permission module | 18k |
| grok | session + agent + sampler | 300k+ |

Working estimate for craze parity: 12–15k lines of Go logic, more at
craze's test ratio. fantasy removes the wire layer (opencode's native
client 9.5k, grok's sampler 31k).

## gx facts the harness inherits

- `~/.grok/providers.toml` (gx-only) and `~/.grok/config.toml`
  (stock-compatible) carry base URL, `api_backend`, keys/`env_key`,
  `context_window`, `stream_tool_calls`, `reasoning_efforts`,
  `supports_vision`, `tool_result_images`.
- Chat completions sends top-level `reasoning_effort`; Messages backend
  maps effort to adaptive thinking; Responses uses `reasoning.effort`.
- GLM 5.3 rejects images anywhere; Meta rejects images inside tool
  messages (gx hoists them into a user message).
