# 07: References

## Clones under `../thirdparty/`

| clone | head (2026-11-11) | read first |
|---|---|---|
| t3code (`pingdotgg/t3code`) | `a21b43fc70` | `apps/server/src/orchestration-v2/Adapters/ClaudeAdapterV2.ts`:<br>• `makeClaudeQueryOptions` 1904-2065<br>• permissions 2963-3040, 3958-4018<br>• `canUseTool` and AskUserQuestion/ExitPlanMode 11122-11328<br>• lifecycle 11590-11717, 12307-12341<br><br>Also:<br>• `apps/server/src/provider/ClaudeProvider.ts` (the auth probe, 441-547)<br>• `apps/server/src/provider/Drivers/ClaudeHome.ts` (env scrubbing)<br>• `packages/provider-acp-registry/src/server/claudeAcp.ts` and `adapter.ts:196-210` (Claude over ACP)<br>• `packages/provider-core/src/server/ProviderAdapter.ts:649-764` (the contract craze's seam echoes) |
| pandacode (`aicodeking/pandacode`) | `106b385` | `src-tauri/src/providers.rs`:<br>• spawn 3286-3338<br>• control messages 1985-2050<br>• the Claude parser 2536-2882<br>• the model probe 1152-1282<br><br>Also, in `src-tauri/src/providers/`:<br>• `steering.rs` (turn end)<br>• `claude_auth.rs`<br>• `warm.rs`<br>• `subagents.rs`<br>• `claude_stream_tests.rs` |
| opencode (`anomalyco/opencode`) | `6c34e24488` | • `specs/v2/claude-agent-provider.md`<br>• `packages/core/src/plugin/provider/claude-agent/` (`claude/query-options.ts`, `claude/session.ts`, `sync/`)<br>• the recording `packages/core/test/plugin/provider/claude-agent/fixtures/recordings/billing-error.jsonl`<br>• history: `git log --oneline -i --grep=anthropic --grep=claude`; key commits `35b03e4cb3`, `a433766a31`, `1ac1a0287c`, `f29e281322`, `958f965d35` |

## The ACP adapter

`@agentclientprotocol/claude-agent-acp` 0.106.0 (npm, 2026-11-10), repo
`github.com/agentclientprotocol/claude-agent-acp`, Apache-2.0. It depends on
`@anthropic-ai/claude-agent-sdk` 0.3.322 and `@agentclientprotocol/sdk` 2.6.0.
To read it, unpack `npm pack` into a scratch directory; it is not cloned
under `../thirdparty/`. Start with:
- `README.md` (subagent sessions, v2);
- `dist/auth-methods.js`;
- `dist/claude-cli.js`;
- `dist/elicitation.js`;
- `dist/permissions/`;
- `dist/v1/acp-agent.js` (steering).

## The local `claude`

`claude` 2.1.322 at `~/.local/bin/claude` (2026-11-11).

**`claude --help` flags that matter:**
- `-p`;
- `--input-format stream-json`, `--output-format stream-json`;
- `--include-partial-messages`, `--replay-user-messages`;
- `--permission-mode` (`acceptEdits`, `auto`, `bypassPermissions`, `manual`,
  `dontAsk`, `plan`);
- `--permission-prompts host|none`;
- `--resume`, `--fork-session`, `--no-session-persistence`;
- `--effort`, `--model`, `--fallback-model`;
- `--forward-subagent-text`.

**`--bare`.** It never reads OAuth or the keychain ("Anthropic auth is strictly
`ANTHROPIC_API_KEY` or apiKeyHelper"), so it is unusable for a subscription
provider.

**`claude auth status`.** It prints JSON with `loggedIn`, `authMethod`,
`apiProvider`, `subscriptionType`, `planTier` and `loginExpiresAt`.

**No ACP mode.** There is no `claude acp` or `--acp`.

## Anthropic pages (fetched 2026-11-11)

- Legal and compliance: https://code.claude.com/docs/en/legal-and-compliance
- Authentication (`setup-token`, precedence): https://code.claude.com/docs/en/authentication
- Headless / `-p` and stream-json: https://code.claude.com/docs/en/headless
- CLI reference: https://code.claude.com/docs/en/cli-reference
- Agent SDK overview (branding, claude.ai login sentence): https://code.claude.com/docs/en/agent-sdk/overview
- Agent SDK on a Claude plan (the June pause, the October update): https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan
- Consumer Terms: https://www.anthropic.com/legal/consumer-terms
- Python SDK transport and control protocol (the de facto wire spec):
  - https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/transport/subprocess_cli.py
  - https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/query.py

## Press and third parties

- The Register, 2026-02-20, on the third-party access clarification.
- Zed blog, 2026-05-14 and 2026-06-16 updates, on the Agent SDK credit and its
  pause: https://zed.dev/blog/anthropic-subscription-changes
- Community Go wrappers of `claude -p`. None is maintained to the standard
  craze would depend on; reference only.
  - `github.com/severity1/claude-agent-sdk-go`
  - `github.com/lancekrogers/claude-code-go`
  - `github.com/humanlayer/humanlayer/claudecode-go`
