# 09 — References

Reference clones live under `../thirdparty/` (sibling of this repo). H0 used
released Go modules from the module cache for its verdicts; a nearby checkout
was never substituted for a release. The four discovery review reports that
produced `05`–`08` are summarized in the owner's memory notes
(`fantasy-harness-discovery`, `harness-reference-review`).

## H0 candidate pins and provenance

| module | exact release | tag commit | module checksum | license | H0 disposition |
|---|---|---|---|---|---|
| `charm.land/fantasy` | `v0.43.2` | `326027229d6a8a37444b98119456df5a4f07ef2c` | `h1:BHnC/iu72aZLG5SE1jIViOIStRUIPwxAlw55TZeKEzc=` | Apache-2.0 plus `NOTICE` | providers and `LanguageModel.Stream` accepted; `Agent.Stream` accepted behind a finish-normalizing `LanguageModel` wrapper (D-21) |
| `charm.land/catwalk` | `v0.52.43` | `aab3ef84556311c61900372867a1a90e20b65f8b` | `h1:VgwwWU7jtUxjgRFOo6pS53epQ2yNvIYaIv0p6sb9mYk=` | MIT | not embedded; a reference when adding models (D-22) |
| `github.com/BurntSushi/toml` | `v1.6.0` | — | recorded in the external probe `go.sum` | MIT | probe-only parser pin; craze already uses this module |

Fantasy's exact release files to read first are `provider.go`, `agent.go`,
`tool.go`, `content.go`, `providers/openaicompat/`, and
`providers/openrouter/`. The streamed agent loop dispatches and continues
only on finish reason `tool-calls` (`agent.go`, the `stepFinishReason ==
FinishReasonToolCalls` checks), and the OpenAI provider's streaming path does
not rewrite `stop` into `tool-calls` the way its non-streaming path does.
External fixture `TestStopWithCompleteToolSeparatesDirectDriverFromAgent`
reproduces the consequence; `followup-2026-09-18/wrap_test.go` in the same
artifact directory shows the wrapper that removes it.

Catwalk's public API used in H0 was `pkg/embedded.GetAll`, with model/provider
fields from `pkg/catwalk/provider.go`. The exact embedded target entries are
under `internal/providers/configs/{fireworks,zai,openrouter}.json`. Its
`cmd/openrouter/main.go` generator derives effort levels/defaults from generic
`reasoning` support; OpenRouter's public `https://openrouter.ai/api/v1/models`
record for MiniMax M3 instead omits `reasoning_effort`, supported efforts, and
a default. gx's corresponding provider preset records that provider contract
and omits all effort fields. H0 did not copy implementation code from either
candidate or reference project.

## Toolchain baseline

- Accepted Fantasy requires Go language version 1.27.0.
- craze therefore uses `go 1.27.0`, `toolchain go1.27.1`, and mise Go
  `1.27.1`.
- `golangci-lint v2.10.1` could not load Go 1.27 export data. The synchronized
  pin is `v2.13.2`, built with Go 1.27.0 and verified against craze under Go
  1.27.1.
- Catwalk requires Go 1.26.6; it is not a dependency, so it does not set the
  floor.
- The toolchain change landed on its own, ahead of any harness code.

## Other libraries

| repo | version | license | why | read first |
|---|---|---|---|---|
| `acp-go-sdk` (`github.com/coder/acp-go-sdk`) | v0.13.5 | Apache-2.0 (`LICENSE`; no root `NOTICE`) | the later ACP server wrapper | `README.md`, `types_gen.go` (`Agent` interface), `extensions.go`, `example/agent/main.go` |

## Harnesses we learned from

| repo | inspected date | license | what it grounded |
|---|---|---|---|
| `crush` | 2026-09-11 | **FSL-1.1-MIT-future** (pointer-only; do not copy code) | how a coding agent sits on fantasy: `internal/agent/agent.go` (callbacks → messages, `PrepareStep` queue fold), `coordinator.go` (provider factory by catwalk type, `buildTools`), `hooked_tool.go` (the wrapper pattern), `agent_tool.go` (sub-agent as a parallel tool), `internal/permission/` |
| `opencode` | 2026-08-30 | MIT | the turn loop (`session/prompt.ts`, `processor.ts`), permissions (`permission/index.ts`, `arity.ts`), tools (`tool/edit.ts` ladder, `tool/truncate.ts`, `tool/shell.ts`), compaction (`session/compaction.ts`), instruction files (`session/instruction.ts`), provider quirks (`provider/transform.ts`), ACP server (`acp/`) |
| upstream `grok-build` (`xai-org/grok-build`) | 2026-09-09 | Apache-2.0 (`LICENSE`; third-party notices in `third_party/NOTICE`) | the reference for craze's current wire; Claude compat (`xai-grok-agent/src/prompt/agents_md.rs`, `discovery.rs`, `plugins/discovery.rs`, `xai-grok-tools/.../skills/`, `xai-grok-workspace/src/permission/claude_settings.rs`, `xai-grok-shell/src/claude_import.rs`), tool taxonomy (`xai-grok-tools/src/tool_taxonomy.rs`, `normalization.rs`), plan mode (`session/acp_session_impl/tool_calls.rs`), sessions (`storage/`, `updates.jsonl`), compaction (`session/compaction.rs`, `xai-compaction-transcript`), sub-agents, the user guide under `xai-grok-pager/docs/user-guide/` (08 skills, 09 plugins, 12 project rules, 19 plan mode, 22 permissions) |
| gx fork (`charliek/grok-build`) at `gx-v1.0.16-gx.12`, commit `66fe38c2e7b155769f72536e9d7a8c1ce1c8be77` | 2026-09-18 | Apache-2.0 (`LICENSE`; third-party notices in `third_party/NOTICE`) | effective gx model/provider configuration |
| `pi` (`earendil-works/pi`) | 2026-09-16 | MIT | the minimal core: `packages/agent/src/agent-loop.ts`, `agent.ts`; `packages/coding-agent/src/core/session-manager.ts`, `core/tools/`, `core/compaction/`, `core/extensions/`, `core/package-manager.ts`; docs `session-format.md`, `extensions.md`, `skills.md`, `packages.md`, `compaction.md`, README "Philosophy" |

Permissive reference code still requires an explicit provenance note and
license/notice handling if later adapted. H0 copied no application
implementation from these projects.

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

The original working estimate for broad craze parity was 12–15k lines of Go
logic, plus tests. H0's probe suggests a craze-owned direct stream loop would
add roughly 1.5–2.5 kLOC plus comparable fixtures; D-21 does not take that
path first. These are planning inputs, not commitments.

## H0 gx facts

- The effective gx source files are `~/.grok/providers.toml` (gx-only) and
  `~/.grok/config.toml` (stock-compatible). H0 read only targeted table data
  and never persisted either file or a credential.
- `providers.toml` recursively overlays the allowlisted `model`,
  `model_providers`, and `auth_provider` tables from `config.toml`; later
  arrays replace earlier arrays. Alias keys may differ from wire model ids.
- Chat completions sends top-level `reasoning_effort`; OpenRouter uses nested
  `reasoning.effort`. Current Z.AI behavior does not establish the earlier
  claimed `thinking` object.
- `stream_tool_calls` controls an optional Responses request field in current
  gx; it does not disable chat-completions tool streaming.
- GLM 5.3 rejects images anywhere. Meta's gateway returns 400 for images
  inside tool messages; gx hoists them into a user message
  (`xai-grok-shell/src/agent/gx_tool_images.rs`). Both are H8 work; the H0
  live scenario was text-only.
- Meta reports reasoning that exhausts `max_tokens` as an empty stream with
  `finish_reason: "stop"` and no usage chunk, not as `length` (D-25).
- The gx alias `fireworks/deepseek-v4-pro` points at a wire id Fireworks no
  longer serves (HTTP 404). `accounts/fireworks/models/deepseek-v4-pro-0813`
  answers; Catwalk lists both ids.
- H0's gx control recorded version `1.0.16+gx.12` but sent zero requests
  because the headless command exposes no enforceable numeric generated-token
  ceiling.
