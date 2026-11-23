---
name: craze
description: "How to hand work to another model through craze from your agent (Claude Code, Cursor, Grok or a craze session). Use it when the user asks to delegate, offload or get a second opinion from a native (craze's own agent), Grok, Cursor, Antigravity (Gemini) or Claude model, to run several models on a task in parallel, to continue or watch a craze session, or to review with another model. Never use craze to reach your own provider (Claude Code is claude, Cursor is cursor, Grok is grok, Antigravity's agy is antigravity, a craze native session is native): use your own sub-agent tool. Covers Claude Code's craze:delegate agent and its presets (sol, astra, muse, wonder, grok, cursor, neon), the craze new --wait, send, wait, cancel and stop commands any agent can run, the claude target, web search, statuses, exit codes, handles, follow-ups, craze attach, and the permission allow-list. Needs craze 0.2.0 or newer."
---

# Delegating to other models with craze

craze drives coding agents in sessions of their own: its own **native** agent (on a model provider's API or a ChatGPT plan), **Grok**, **Cursor**, **Antigravity** (Google's `agy`, on the user's Antigravity plan: Gemini 4 Neon, Argon and Hydrogen among its models) and **Claude** (the user's own Claude Code, on their Claude plan). This skill is how your agent — Claude Code, Cursor, Grok or a craze session — hands a task to one of those sessions and gets the result back, as it would from a sub-agent of its own. From Claude Code the plugin's agents do it; any agent can run the commands itself ([Driving craze directly](#driving-craze-directly)). Needs craze 0.2.0 or newer on `PATH`.

## Don't call yourself

craze is for reaching a **different** provider. Never use it to start a session on the provider you already run on; your own sub-agent tool does that better:

| You are | Your provider | Use instead of craze |
|---|---|---|
| Claude Code | `claude` | the Agent tool |
| Cursor | `cursor` | Cursor's own sub-agents |
| Grok | `grok` | Grok's own sub-agents |
| Antigravity (`agy`) | `antigravity` | agy's own sub-agents |
| a craze native session | `native` | the `agent` tool, which runs any native model, the `sol`, `astra`, `muse` and `wonder` models included: use `agent`, not `craze:sol` |

craze holds to this where it can tell who is calling. `craze new` refuses its caller's own provider, exit 2: `craze: this agent already runs on <provider>; use your own sub-agent tool for <provider> work, not craze (--allow-same-provider overrides)`. It knows a craze session by `CRAZE_SESSION_PROVIDER` (craze sets it for every agent it starts, native's shell included), Claude Code by `CLAUDECODE=1`, a standalone cursor-agent by `CURSOR_AGENT` and a standalone agy by `ANTIGRAVITY_AGENT`; when two of those three are set and there is no `CRAZE_SESSION_PROVIDER`, one started the other and craze cannot tell who is calling, so it does not refuse. Grok sets no marker craze trusts, so from Grok the rule is yours alone to keep. Relay a refusal as it is; do not retry, and do not add `--allow-same-provider`, which is for a person who means it. `craze send`, `wait` and `cancel` are not checked.

## The agents (Claude Code)

In Claude Code, dispatch them with the Agent tool, `subagent_type` `craze:<name>`. The prompt is the task.

| Agent | Runs | Use for |
|---|---|---|
| `craze:delegate` | the provider and model **you** name | anything the presets do not cover |
| `craze:sol` | native `chatgpt/gpt-6.2-sol`, effort high | hard coding, debugging, review |
| `craze:astra` | native `chatgpt/gpt-6.2-astra`, effort high | the hardest design and review questions |
| `craze:muse` | native `muse-spark-1.3-contributor` | routine, bounded tasks, cheaply |
| `craze:wonder` | native `muse-wonder-1.0`, effort high | hard tasks, at about three times Spark's price per token |
| `craze:grok` | grok `grok-4.7`, effort high | Grok's own agent |
| `craze:cursor` | cursor `composer-2.5` | Cursor's agent |
| `craze:neon` | antigravity `gemini-4-neon`, effort high | Antigravity's agent (Gemini 4 Neon); no `mode=ask` |

There is no `craze:claude`: Claude Code is the only agent that loads these, and it must not target claude.

- `craze:delegate` takes a first line `target: <provider> [<model>] [effort=<low|medium|high|xhigh|max>] [mode=plan|ask] [web=on|off]`, then the task. Providers: `native`, `cursor`, `grok`, `claude`, `antigravity`; `web=` is for `native` only, and `mode=ask` is not available on `claude` or `antigravity`. Example: `target: native chatgpt/gpt-6.2-sol effort=high mode=plan`. From Claude Code itself craze refuses `claude` (above).
- The native presets (`sol`, `astra`, `muse`, `wonder`) take an optional first line of `mode=plan` or `mode=ask` and `web=on` or `web=off`, in either order (`mode=plan web=on`), then the task. A first line made only of `key=value` words with a bad or repeated `mode` or `web` is answered with an error and nothing runs. `craze:grok` and `craze:cursor` take `mode=` only and answer an error to `web=`. `craze:neon` takes `mode=plan` only: it answers `mode=ask` and `web=` with an error and runs nothing.
- `craze:sol` and `craze:astra` run on a ChatGPT plan sign-in. With an OpenAI API key instead (billed per token; a craze newer than 0.2.0), use `craze:delegate` with `target: native openai/gpt-6.2-sol effort=high` (or `openai/gpt-6.2-astra`). The GPT-6.2 models need a craze newer than 0.3.0.
- To continue a session, send the agent a follow-up with SendMessage: it runs `craze send` on the same session, and the model keeps its context. The agent forwards every message you send it, a recap or a question about an earlier turn included; it never answers from the earlier results.

## The targets

| Provider | Runs | Notes |
|---|---|---|
| `native` | craze's own agent, on a model provider's API or a ChatGPT plan | `--model` names a native model (`chatgpt/gpt-6.2-sol`, `muse-spark-1.3-contributor`, …); the only one with web search |
| `grok` | the Grok CLI's agent | |
| `cursor` | the Cursor agent | |
| `antigravity` | Google's Antigravity agent (`agy`), on the user's Antigravity plan | `--model` names a Gemini or other agy model (`gemini-4-neon`, `gemini-4-hydrogen`, …); no `mode=ask` (`--ask`), no web search. `mode=plan` returns agy's plan file and message; agy's plan discipline is its own, not craze's enforcement. Sub-agents agy launches asynchronously may be cut off when the unattended turn ends |
| `claude` | the user's own Claude Code, on their Claude Pro or Max plan | needs a craze newer than 0.3.0; no `mode=ask` (`--ask`), no web search, no `--fast` (it bills extra usage). From an agent that does not run on claude (Cursor, Grok, a native, cursor or grok craze session), run it directly: `craze new --wait --timeout 9m --max-message 20000 --provider claude --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file` |

- **Web search** (`web=` on the agents, `--web-search` on the command; native only; needs a craze newer than 0.2.0). Set `web=on` when the task needs information newer than a model's training or not in the repository: current library or API documentation, release notes and changelogs, looking up an error message, comparing external tools. Leave it off for edits, refactors and reviews of local code. The queries and fetched URLs go to Exa, so do not set it for work the user has called confidential. With no `web=`, the home's own setting applies: off for an unattended run unless the owner turned it on. `web=off` guarantees the session has no web tools. The choice is fixed when the session is created; a follow-up cannot change it. The result's `webSearch` object (with `craze new --wait --json`) reports what was searched and fetched. On an older craze the agent answers `this craze does not support web search; update craze, or run again without web=` and does not retry.
- The task must stand alone. The session has not seen this conversation: give it the goal, the paths, the constraints and what to return. Put a large context (a diff, a log, a spec) in a file in the project and name the file in the task.
- The session works in the **current directory**, with its own model's tools, instructions and permissions.

## What it can and cannot do

- **Modes.** Without a mode the session can edit files and run commands. `mode=ask` refuses edits **and** the shell (not available on claude). `mode=plan` refuses edits except the agent's own plan file but **lets the shell through**, and the turn **ends at the plan**: the plan comes back in the result and nothing is implemented (on every provider). The session stays in plan mode, follow-ups and reloads included: to implement the plan, start a new delegation without `mode=plan` and put the plan in its task. A task that says "read-only" or "do not edit" is a request, not enforcement.
- **Whose rules apply.** Claude Code's permission rules and hooks do not apply to a native, Grok or Cursor session's tools; only the mode above limits it. A **claude** session is the user's own Claude Code, so it runs under their Claude Code settings, hooks and permission rules (the project's included). Do not delegate work you would not let that model do unattended.
- **Unattended.** The session answers its own questions with the first option, returns its plan instead of accepting it, and runs no background work: a turn is whole when it ends. On claude, a background sub-agent is waited for, but a background shell command (`run_in_background`) is **not**: the turn ends once the command starts, and the result can come before it finishes. An approval request (only under `--no-force`) shows up as `needs_input`.
- **Time.** A call waits up to 9 minutes (`--timeout 9m`; in Claude Code the Bash call's `timeout` is `600000`). A longer task comes back as `running` with a handle, to follow with `craze wait`.
- **Cost.** The footer shows what a native turn cost (`unpriced` for a plan sign-in). claude is unpriced too: it runs on the user's Claude plan and counts against its limits. Cursor and Grok report none.

## Permissions (Claude Code, default permission mode)

The agents run `craze` and write the task file. To avoid a prompt on each call, allow:

```json
{
  "permissions": {
    "allow": [
      "Bash(craze new:*)",
      "Bash(craze send:*)",
      "Bash(craze wait:*)",
      "Bash(craze cancel:*)",
      "Edit(//tmp/craze-delegate-*)"
    ]
  }
}
```

Claude Code applies `Edit` rules to its Write tool, and `//` marks an absolute path (a single leading `/` is relative to the settings file's own directory).

## Running several in parallel

From Claude Code, dispatch several agents in one message, each in the background (`run_in_background`); each runs its own `craze new --wait` with a Bash `timeout` of `600000`. Their results arrive as each finishes. Give each a different task or a different model; do not point two at the same files to edit.

## Driving craze directly

This is the path for every agent without Claude Code's Agent tool (Cursor, Grok, a craze session), and for your own Bash calls in Claude Code. **Every shell call that waits must be allowed ten minutes** (in Claude Code, Bash `timeout: 600000`; its default is 120 s) and use `--timeout 9m` under it; add `--max-message 20000` so a long answer does not overflow the Bash output (the rest goes to a file named in the output).

```bash
craze new --wait --timeout 9m --max-message 20000 --provider native --model chatgpt/gpt-6.2-sol --effort high --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze new --wait --timeout 9m --max-message 20000 --provider claude --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze send 175bfe79 --wait --timeout 9m --max-message 20000 --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze wait 175bfe79 --from <handle> --timeout 9m --max-message 20000
```

Never name your own provider in `--provider` ([above](#dont-call-yourself)); the claude line is for an agent that does not run on claude (Cursor, Grok, a native, cursor or grok craze session), and needs a craze newer than 0.3.0. Write the prompt to a file (Claude Code's `Write`, or your own file tool) and pass it with `--prompt-file`. Keep the command plain: no variables, `$(...)`, heredoc or `||`. The full command list, statuses, exit codes and handles are in [references/commands.md](references/commands.md).

## Watching and stopping

- `craze attach --session <short>` opens the session's TUI (the footer's last line prints the exact command). It is how a person answers a `needs_input` ask or watches the work.
- `craze ps` lists the machine's sessions.
- `craze cancel <short>` cancels its running turn; the session goes on. `craze stop <short>` ends the session.
- A session a delegation started is marked in the index: `craze -c` skips it, and the resume picker and session list dim it.
