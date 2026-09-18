# 09 — References

Reference clones live under `../thirdparty/` (sibling of this repo). H0 used
released Go modules from the module cache for its verdicts; a nearby checkout
was never substituted for a release. The four discovery review reports that
produced `05`–`08` are summarized in the owner's memory notes
(`fantasy-harness-discovery`, `harness-reference-review`).

## H0 candidate pins and provenance

| module | exact release | tag commit | module checksum | license | H0 disposition |
|---|---|---|---|---|---|
| `charm.land/fantasy` | `v0.43.2` | `326027229d6a8a37444b98119456df5a4f07ef2c` | `h1:BHnC/iu72aZLG5SE1jIViOIStRUIPwxAlw55TZeKEzc=` | Apache-2.0 plus `NOTICE` | public providers and `LanguageModel.Stream` accepted; released `Agent.Stream` rejected |
| `charm.land/catwalk` | `v0.52.43` | `aab3ef84556311c61900372867a1a90e20b65f8b` | `h1:VgwwWU7jtUxjgRFOo6pS53epQ2yNvIYaIv0p6sb9mYk=` | MIT | evaluated and rejected as H1's catalog |
| `github.com/BurntSushi/toml` | `v1.6.0` | — | recorded in the external probe `go.sum` | MIT | probe-only parser pin; craze already uses this module |

Fantasy's exact release files to read first are `provider.go`, `agent.go`,
`tool.go`, `content.go`, `providers/openaicompat/`, and
`providers/openrouter/`. The deterministic released-agent limitation is
captured by external probe fixture
`TestStopWithCompleteToolSeparatesDirectDriverFromAgent`: the direct driver
continues a complete `stop` tool call for three requests; released Agent
stops after one request without dispatching it.

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
- Catwalk requires Go 1.26.6, but its rejection means it does not determine
  the production dependency floor.

## Other libraries

| repo | version | license | why | read first |
|---|---|---|---|---|
| `acp-go-sdk` (`github.com/coder/acp-go-sdk`) | v0.13.5 | Apache-2.0 (`LICENSE`; no root `NOTICE`) | the later ACP server wrapper | `README.md`, `types_gen.go` (`Agent` interface), `extensions.go`, `example/agent/main.go` |

## Harnesses we learned from

| repo | inspected date | license | what it grounded |
|---|---|---|---|
| `crush` | 2026-09-11 | **FSL-1.1-MIT-future** (pointer-only; do not copy code) | how a coding agent sits on Fantasy: provider factory, tool wrappers, permissions, and sub-agent shape |
| `opencode` | 2026-08-30 | MIT | turn loop, permissions, edit/truncation/shell tools, compaction, instructions, provider transforms, ACP server |
| upstream `grok-build` (`xai-org/grok-build`) | 2026-09-09 | Apache-2.0 (`LICENSE`; third-party notices in `third_party/NOTICE`) | craze's current behavior: Claude compatibility, tool taxonomy, plan mode, sessions, compaction, sub-agents, and permissions |
| gx fork (`charliek/grok-build`) at `gx-v1.0.16-gx.12`, commit `66fe38c2e7b155769f72536e9d7a8c1ce1c8be77` | 2026-09-18 | Apache-2.0 (`LICENSE`; third-party notices in `third_party/NOTICE`) | effective gx model/provider configuration |
| `pi` (`earendil-works/pi`) | 2026-09-16 | MIT | minimal agent loop, session tree, tools, compaction, extensions, skills, and package management |

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
logic, plus tests. H0 adds a rough 1.5–2.5 kLOC estimate, plus comparable
fixtures, for the craze-owned direct stream loop before later tools and
permissions. These are planning inputs, not commitments.

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
- GLM 5.3 rejects images; Meta tool-result image placement remains deferred
  to H8. The approved H0 live scenario was text-only.
- H0's gx control recorded version `1.0.16+gx.12` but sent zero requests
  because the headless command exposes no enforceable numeric generated-token
  ceiling.
