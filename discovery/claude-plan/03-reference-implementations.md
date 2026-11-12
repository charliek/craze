# 03: Reference implementations

Four codebases drive a subscription-signed-in Claude today. None of them runs
OAuth of its own any more; all of them spawn a `claude` binary the user signed
in to. They differ in which wire they speak to it and how much of Claude Code's
harness they keep. Paths are relative to each clone under `../thirdparty/`, at
the heads named in the README. Line numbers are from reading on 2026-11-11.

## At a glance

| | t3code | pandacode | opencode "Claude Agent" | `claude-agent-acp` |
|---|---|---|---|---|
| Language | TypeScript (Effect) | Rust (Tauri) | TypeScript (Bun) | TypeScript (Node ≥22 or a bun-compiled binary) |
| Wire to `claude` | Agent SDK `query()`, `@anthropic-ai/claude-agent-sdk` 0.3.321 | hand-rolled stream-json + control protocol | Agent SDK `query()` | Agent SDK 0.3.322 |
| Wire to the app | in-process | in-process | in-process | **ACP v1** (v2 draft opt-in) |
| Process per | session (long-lived, streaming input) | **turn** (`--resume` next turn, warm spare) | session (5 min idle) | session |
| Whose tools | Claude Code's | Claude Code's | **opencode's**, via an MCP bridge; Claude Code's off | Claude Code's |
| Permissions | `canUseTool` → UI; `acceptForSession` → `updatedPermissions` | `--permission-prompt-tool stdio` → `can_use_tool` → allow/deny only | `dontAsk` + opencode's own prompts | ACP `session/request_permission` |
| AskUserQuestion | intercepted in `canUseTool`, answered via `updatedInput.answers` | auto-denied ("ask in your response") | n/a (Claude's tools off) | ACP **elicitation** |
| ExitPlanMode | captured as a proposed plan, tool denied "stop here" | not special: an ordinary approval | not used; opencode's plan agent | `exit-plan.js` (permission-shaped) |
| Sub-agents | `task_*` system frames + `parent_tool_use_id` routing to child threads | `parent_tool_use_id` routed to per-child parsers | n/a | `subagent_update` child sessions when the client declares the capability, else Agent/Task tool calls |
| Models | bundled manifest + `init.models` | `initialize` response `models` (a no-inference probe process) | `init.models` + `getContextUsage` probe | ACP config options |
| Auth check | SDK `initializationResult().account` (`tokenSource`, `subscriptionType`) | `claude auth status --json` | `initializationResult().account` | advertises terminal auth methods (`claude auth login --claudeai`, `--console`) |
| Reads `.credentials.json` | **yes**, for the usage meter and rate-limit reset credits | **yes**, for the usage meter | no ("never reads Claude Code's saved login") | no |
| Says anything about Anthropic's terms | no; FAQ answers "Yes" to subscription use | no; "under that provider's own terms" | yes; spec asks "Do we need … approval?"; docs page unlisted, provider still ships (v2.2.7) | n/a |
| Size | adapter 13,313 lines; test 21,847; testkit 2,850; 85 replay fixtures | parser ~350 + turn-end ~675 + shared sub-agents/warm/auth ~1.7k; 79 stream tests | spec + plugin, five fixes in 11 days after launch | 14.8k lines of JS in `dist/`; releases near daily (0.106.x) |

## t3code: the Agent SDK, one long-lived process per thread

`apps/server/src/orchestration-v2/Adapters/ClaudeAdapterV2.ts` (`A` below).

- `query({prompt, options})` with `prompt` an async iterable fed from a queue
  (`A:1303-1309`, `A:1416-1419`), so one `claude` per thread, turns offered into
  it (`A:1545`). t3code supplies its own `spawnClaudeCodeProcess` (a detached
  process group and an fd-3 watchdog, `A:1355-1412`).
- Options worth copying (`A:1904-2065`):
  - `includePartialMessages`;
  - `thinking: {type: "adaptive", display: "summarized"}`;
  - `systemPrompt: {preset: "claude_code", append}`;
  - `tools: {preset: "claude_code"}`;
  - `permissionMode`, `canUseTool`, `sessionId` for a new thread and `resume`
    after;
  - `mcpServers`, sent after init with `setMcpServers()` to keep secrets out of
    argv.
- Lifecycle:
  - Interrupt is `query.interrupt()`, then close after 10 s (`A:12307-12341`).
  - A model change is live (`setModel()`, `applyFlagSettings` for effort and
    fast, `A:11590-11605`).
  - A **permission-mode change restarts the process with `resume`**
    (`A:3174-3201`).
  - Steer is a user message with `priority: "now"` (`A:12486`).
  - Rollback stores a message uuid for `resumeSessionAt` (`A:13084-13140`).
- Its provider contract (`packages/provider-core/src/server/ProviderAdapter.ts:649-764`)
  is what craze's `agent.Session` was modelled on. The event union is projected
  state (`node.updated`, `subagent.updated`, `runtime_request.updated`,
  `plan.updated`, `turn.terminal`), not wire deltas (`PA:83-204`).
- Mode mapping (`A:2963-3010`): plan→`plan`, approval-required→`default`,
  auto-accept-edits→`acceptEdits`, auto→`auto`, full-access→`bypassPermissions`,
  read-only→`dontAsk`.
- t3code **also** runs Claude over ACP: its generic ACP-registry driver
  recognises `claude-agent-acp` and forces `CLAUDE_AGENT_ACP_EXPERIMENTAL_V2=1`
  (`packages/provider-acp-registry/src/server/adapter.ts:196-210`). It maps
  Agent/Task tool calls and `subagent_update` into sub-agent rows
  (`packages/provider-acp-registry/src/server/claudeAcp.ts`, 117 lines). That is
  the closest prior art to route A.
- Gotchas in its comments:
  - Iterate the `Query`, not its raw generator, or interrupt deadlocks (`A:969`).
  - Boot is unbounded, so it uses a 5 min boot timeout (`A:346-348`).
  - A fresh `sessionId` on an existing transcript fails "already in use"
    (`A:11713`).
  - Background "wake" turns interleave with the prompt's own (`A:10916-11064`).
  - CLI notices arrive as assistant messages with model `<synthetic>`
    (`A:9837`).
  - A settings.json `env` beats the process env (`A:1930`).
  - Behaviour is pinned to Claude Code 2.1.292, 2.1.305 and 2.1.310 in places.

## pandacode: the control protocol by hand, in Rust

This is the most useful reference for a Go port (route B), because it is the
protocol without an SDK in between. `src-tauri/src/providers.rs` (`P` below).

- Spawn line (`P:3286-3338`):

  ```
  claude -p --input-format stream-json --output-format stream-json
    --include-partial-messages --verbose --permission-prompt-tool stdio
    --replay-user-messages [--include-hook-events]
    --permission-mode <m> [--model X] [--effort E] [--resume=<id>]
    [--append-system-prompt <text>] [--mcp-config <json>]
    --disallowedTools=ScheduleWakeup,Monitor,CronCreate
  ```

  `CLAUDECODE` is removed from the environment. In "Subscription" mode the
  `ANTHROPIC_*`, `CLAUDE_CODE_OAUTH_TOKEN` and cloud-provider variables are
  removed too, so the saved login wins (`src-tauri/src/providers/claude_auth.rs:11-61`).
- On stdin:
  - `control_request{subtype:"initialize", hooks:null}`, then the user message
    `{"type":"user","message":{"role":"user","content":…},"parent_tool_use_id":null,"session_id":…}`
    (`P:1985`, `P:2573-2580`);
  - `control_request{subtype:"interrupt"}` (`P:2050`);
  - `control_response` to `can_use_tool` with
    `{"behavior":"allow","updatedInput":…}` or `{"behavior":"deny","message":…}`
    (`P:2034`);
  - an error response to any other inbound control subtype (`P:2820`).
- On stdout it handles:
  - `system` (`init`, `compact_boundary`, `status`, `task_*`, `hook_*`,
    `commands_changed`, `local_command_output`);
  - `stream_event` (`message_start`/`_delta`, `content_block_*`, `text_delta`,
    `thinking_delta`, `input_json_delta`);
  - `assistant`, `user` (tool_result, replay echoes);
  - `control_request` `can_use_tool`, `control_cancel_request`;
  - `rate_limit_event`, `result` (`P:2536-2882`).
- Models without inference: a throwaway process sends only `initialize`. The
  response's `models[]` carries `value`, `displayName`, `resolvedModel`,
  `supportedEffortLevels` and `contextWindow` (`P:1152-1282`). The init
  response can exceed 256 KiB with many plugins, so the cap is now 4 MiB.
- **Turn end is not `result`** (`src-tauri/src/providers/steering.rs:14-31`, `392-433`):
  - a resumed process can emit an empty `result` first;
  - each background agent triggers its own hand-back turn;
  - a local slash command gives `num_turns == 0`.
  This is the hardest part of the port, about 675 lines alone.
- One process per turn costs about 1.4 s, which it hides with a pre-spawned warm
  spare (`src-tauri/src/providers/warm.rs`). It must disallow `ScheduleWakeup`,
  `Monitor` and `CronCreate` because its process ends with the turn.
- It asks nothing about mode or model mid-session: they are flags on the next
  turn's process.

## opencode: from OAuth spoofing to "Claude Agent"

- **History.** Covered in [02](02-policy-and-billing.md). Built-in OAuth
  (2025-06), moved to a plugin (2025-08), removed on legal request (2026-03).
  Every condition for acceptance was imitation of Claude Code (its client id,
  its betas, a "You are Claude Code" prefix, no `User-Agent`). That is exactly
  what craze must never do.
- **Claude Agent** (`packages/core/src/plugin/provider/claude-agent/`, spec
  `specs/v2/claude-agent-provider.md`, 2026-10-30):
  - Runs the user's own `claude` through the SDK, but as a *model only*:
    `tools: []`, `settingSources: []`, `permissionMode: "dontAsk"`, and
    opencode's tools offered through an in-process MCP server (`mcp__opencode__*`).
  - Each tool call is parked until opencode has run it under its own
    permissions.
  - The cost is serial tool calls, a restart whenever the tool set changes, and
    the SDK at ~270 MB.
  - Its spec's open questions are craze's too: "Do we need to discuss anything
    here with anthropic or any kind of approval?", and reports that SDK users
    get less plan usage.
  - It is the source of the 2026-10-17 "Third-party apps now draw from your
    extra usage" recording.

## `claude-agent-acp`: the ACP adapter

`@agentclientprotocol/claude-agent-acp` 0.106.0 (2026-11-10), Apache-2.0,
repo `agentclientprotocol/claude-agent-acp`. It was formerly Zed's
`@zed-industries/claude-code-acp`. Inspected from the npm tarball in the
session scratchpad. It is not vendored.

- **Runs:** Node ≥22 (`bin: claude-agent-acp`), or a standalone bun-compiled
  release that ships `claude` beside it. The `claude` it spawns is chosen in
  this order:
  1. `CLAUDE_CODE_EXECUTABLE`;
  2. the one beside the binary;
  3. the SDK's platform package (`dist/claude-cli.js`).

  craze would set `CLAUDE_CODE_EXECUTABLE` to the user's own `claude`.
- **Auth:**
  - It advertises *terminal* auth methods: `claude-ai-login` runs
    `claude auth login --claudeai`, and `console-login` runs `--console`. On
    SSH or `NO_BROWSER` it runs `claude /login` instead (`dist/auth-methods.js`).
  - craze's ACP client has no terminal-auth support. A craze session would
    instead require a prior `claude auth login` and show the hint, as it does
    for `agent login` and `grok login`.
- **ACP v1 surface:**
  - `session/new`, `session/load` (with replay), `session/prompt`,
    `session/cancel`, `session/set_mode`;
  - `session/set_config_option`, with models, effort and mode as config options;
  - `session/request_permission`;
  - `session/update` kinds `agent_message_chunk`, `agent_thought_chunk`,
    `tool_call`, `tool_call_update`, `plan` and `current_mode_update`;
  - also `config_option_update`, `session_info_update` and `usage_update`;
  - also `compaction_update`, `notice` and `subagent_update`;
  - **elicitation** for AskUserQuestion (`dist/elicitation.js`).
- **Sub-agents:** a client that declares `clientCapabilities.subagents` gets
  each Agent or Task as a child session, announced by `subagent_update`. Its
  states are `running`, `requires_action` and `idle`, with a stop reason. A
  client that does not declare it sees them as plain tool calls.
- **v2 draft:** `CLAUDE_AGENT_ACP_PROTOCOL=v1|v2|auto`. It adds
  `session/resume`, `session/fork`, `session/list` and `session/set_title`,
  but "the draft can still change", and steering is v1 only.
