# 08: CL0 results (2026-11-11)

The probes CL0 named in [05](05-roadmap.md) were run on 2026-11-11 with the
owner's go-ahead. Setup:
- the owner's own Claude plan login, `claude` 2.1.322 and
  `@agentclientprotocol/claude-agent-acp` 0.106.0 (installed with
  `--ignore-scripts --omit=optional`, so the SDK's bundled `claude` was absent
  and the owner's binary ran through `CLAUDE_CODE_EXECUTABLE`), Node 24.22.1;
- the probes ran from throwaway directories, with the inherited `CLAUDE*`
  variables stripped (`env -i` plus `HOME`, `USER`, `LANG`, `PATH`);
- the scripts were two Python drivers, one for stream-json and one for
  ACP v1, plus a shell wrapper that let unmodified craze 0.2.0 run the adapter
  in place of `cursor-agent`.

Logs and scripts stayed in the session scratchpad and are not committed; they
carry account identifiers. Spend: about a dozen small turns, mostly
`claude-haiku-5-5`, with one plan-mode turn on `claude-sonnet-5-5`. The
API-equivalent cost Claude Code reported was about $0.12 in total. The plan's
seven-day utilization read 0.45 before and after.

## Probe 1: billing

| route | turns | `rate_limit_info` on the stream | result |
|---|---|---|---|
| B: `claude -p --input-format stream-json --output-format stream-json` with `--permission-prompt-tool stdio` | read, then write (one `can_use_tool`, answered allow) | `status: allowed`, `isUsingOverage: false`, `overageStatus: rejected`, `overageDisabledReason: org_level_disabled`, seven-day window | success, `apiKeySource: none` |
| A: `claude-agent-acp` over ACP v1 | twelve prompts over four sessions (one of them run by craze 0.2.0), plus two background wake turns | the same object, carried in `usage_update._meta["_claude/rateLimit"]` | success on every turn |

**Answer: yes for both routes on this account, as of 2026-11-11.** Both drew
from plan limits. Extra usage is disabled at the org level here, so a turn that
Anthropic classed as "third-party → extra usage" would have failed as
opencode's recording did. None did. opencode's 2026-10-17 failure is therefore
not universal. It may be specific to opencode's configuration (no Claude Code
tools, `verbatimPrompts`), to its account, or to a state since reverted. Keep
the check in the product: a billing or overage error must be typed, and
`isUsingOverage: true` should show.

## Probe 2: adapter fit

### What the adapter does, by feature

| feature | what was seen | what craze needs |
|---|---|---|
| Initialize | `loadSession: true`; `promptCapabilities.image`; `_meta.steering.supported`; **`authMethods: []`** (craze declares no terminal auth); then `auth/status_update` and `_auth/status_update` notifications naming the plan | Treat no auth methods as "already signed in"; read the plan label from `auth/status_update`. craze's own `claude auth status --json` pre-check stays the gate. |
| Modes | `default` (named "Manual"), `acceptEdits`, `plan`, `auto`, `bypassPermissions`. **New sessions start in `auto`** (classifier approves; no permission cards). | Set the mode explicitly after `session/new`. `--no-force` should mean `default`. Add `auto` to the mode table. |
| Config options | `mode`, `model` (`default`, `opus`, `fable`, `sonnet`, `haiku` and pinned ids), `effort` (category `thought_level`: `default` and `low`–`max`), `fast` and `ultracode` (category `model_config`), `agent` (the user's Claude Code agents, the craze plugin's included) | The parameterized picker already reads them: craze 0.2.0 set `haiku` with no change. |
| Permission | `session/request_permission` with `allow_once`, `allow_always` and `reject_once` options. Write: `allow-once`, `allow-with-updates` ("allow all edits during this session"), `reject`. | Already handled by craze's permission card. |
| ExitPlanMode | A permission titled "Approve Plan", tool kind `switch_mode`, options `exit-plan-default` (approve, manual edits), `exit-plan-clear-auto`, `exit-plan-auto`, `exit-plan-bypass`, `reject` ("No, keep planning"). Approval sent `current_mode_update` → `default`, then the write asked its own permission. | Dialect: recognise `_meta.claudeCode.toolName == "ExitPlanMode"` and raise the plan card; map approve → `exit-plan-default`, keep planning → `reject`. |
| AskUserQuestion | **Without `clientCapabilities.elicitation.form` the tool is removed from Claude's tool set** (Claude said it was unavailable). With it: an `elicitation/create` form, one `oneOf` property per question (`question_0`) plus a free-text `question_0_custom`. Answered with `{action: "accept", content}`. | Declare `elicitation.form` and render the form as a question card. Answers go back as `content`. |
| Sub-agents | Without `clientCapabilities.subagents`: an Agent call is a `tool_call` titled "Task", kind `think`, `_meta.claudeCode.toolName: "Agent"`, and the child's tool calls carry `_meta.claudeCode.parentToolUseId`. With it: `subagent_update` on the parent (child `sessionId`, title, description, `capabilities.cancel`, `state` running → idle with `end_turn`), and the child's own `session/update` stream under the child id. | Either works with craze's current routing. The capability form matches grok's child stream. |
| Cancel | `session/cancel` during generation: the prompt returned `stopReason: cancelled` within the second, with a zero `_meta.quota`. | Already handled. |
| Steering | `_session/steering` mid-turn replied `{outcome: "injected"}`, and the model used the steered text in the same turn. | `Interject` maps onto it, as grok's `_x.ai/interject` does. |
| Background work and wakes | Claude Code moved a long `sleep` to the background on its own. A background Agent or Bash that finished **after** the prompt returned started **a turn of its own with no prompt in flight**: text chunks, then a closing `usage_update` with `_meta["_claude/origin"].kind == "task-notification"`. No turn-start marker. | Handle it as a foreign turn (`ForeignTurn`), as native's wake is: text arriving with no prompt in flight opens one, and the origin-tagged closing usage ends it. |
| Usage | `usage_update` with `used`/`size` (context: 1,000,000), the rate-limit object on the first update of a turn, `cost` (API-equivalent USD) on the last. The prompt result carries `_meta.quota` (tokens, per model). | Read it into the usage part. Show the cost as unpriced on a plan, and the seven-day utilization if wanted. |
| Load | `session/load` in a new process replayed `user_message_chunk`, `agent_message_chunk`, `tool_call`(`_update`), `subagent_update`, `session_message` and the child's frames. The mode was restored. **The model was not: it came back as the default (`fable`).** | Re-apply the session's model after a load. |
| Other updates | `session_info_update` (titles), `available_commands_update` (Claude Code's slash commands and skills), `config_option_update`; stderr is progress logging. | Titles already shown. Commands go to the slash catalog. Show stderr only on failure. |

### Unmodified craze 0.2.0

`craze prompt --provider cursor --agent-bin <wrapper> --model haiku --no-force --json`,
with the wrapper running the adapter in cursor-agent's place, completed a turn
cleanly. The model was set, and the title and tool rows were shown with diffs
for the write. Text streamed and the turn ended `end_turn`. The adapter's
`auto` mode meant no permission was asked. So route A's floor, an unmodified
craze, already works; CL1 is the list in the "craze needs" column above.

## Verdicts

- **Billing:** both routes draw from plan limits on the owner's account today.
- **Route A fits.** Every gap is a dialect item in a known place.
- **Route B is proven at the protocol level**, as a fallback. `initialize`
  returns the account, the model list with effort levels, and slash commands
  with no inference. `can_use_tool` works as pandacode documents it.
