---
name: craze
description: How to hand work to other models through craze from Claude Code. Use it when the user asks to delegate, offload or get a second opinion from a native (craze's own agent), Grok or Cursor model, to run several models on a task in parallel, to continue or watch a craze session, or to review with another model. Covers the craze:delegate agent and its presets (sol, astra, muse, grok, cursor), the craze new --wait, send, wait, cancel and stop commands, statuses, exit codes, handles, follow-ups, craze attach, and the permission allow-list. Needs craze 0.2.0 or newer.
---

# Delegating to other models with craze

craze drives other coding agents (its own **native** agent on a model provider's API or a ChatGPT plan, **Grok**, **Cursor**) in sessions of their own. This plugin lets Claude Code hand a task to one of those sessions and get the result back, as it would from a native sub-agent. Needs craze 0.2.0 or newer on `PATH`.

## The agents

Dispatch them with the Agent tool, `subagent_type` `craze:<name>`. The prompt is the task.

| Agent | Runs | Use for |
|---|---|---|
| `craze:delegate` | the provider and model **you** name | anything the presets do not cover |
| `craze:sol` | native `chatgpt/gpt-6.1-sol`, effort high | hard coding, debugging, review |
| `craze:astra` | native `chatgpt/gpt-6-astra`, effort high | the hardest design and review questions |
| `craze:muse` | native `muse-spark-1.3-contributor` | routine, bounded tasks, cheaply |
| `craze:grok` | grok `grok-4.7`, effort high | Grok's own agent |
| `craze:cursor` | cursor `composer-2.5` | Cursor's agent |

- `craze:delegate` takes a first line `target: <provider> [<model>] [effort=<low|medium|high|xhigh|max>] [mode=plan|ask]`, then the task. Providers: `native`, `cursor`, `grok`. Example: `target: native chatgpt/gpt-6.1-sol effort=high mode=plan`.
- The presets take an optional first line `mode=plan` or `mode=ask`, then the task.
- The task must stand alone. The session has not seen this conversation: give it the goal, the paths, the constraints and what to return. Put a large context (a diff, a log, a spec) in a file in the project and name the file in the task.
- The session works in the **current directory**, with its own model's tools, instructions and permissions.
- To continue a session, send the agent a follow-up with SendMessage: it runs `craze send` on the same session, and the model keeps its context. The agent forwards every message you send it, a recap or a question about an earlier turn included; it never answers from the earlier results.

## What it can and cannot do

- **Modes.** Without a mode the session can edit files and run commands. `mode=ask` refuses edits **and** the shell. `mode=plan` refuses edits except the agent's own plan file but **lets the shell through**, and the turn **ends at the plan**: the plan comes back in the result and nothing is implemented (on every provider). The session stays in plan mode, follow-ups and reloads included: to implement the plan, start a new delegation without `mode=plan` and put the plan in its task. A task that says "read-only" or "do not edit" is a request, not enforcement.
- **No Claude Code rules inside.** Claude Code's permission rules and hooks do not apply to the craze session's tools; only the mode above limits it. Do not delegate work you would not let that model do unattended.
- **Unattended.** The session answers its own questions with the first option, returns its plan instead of accepting it, and runs no background work: a turn is whole when it ends. An approval request (only under `--no-force`) shows up as `needs_input`.
- **Time.** A call waits up to 9 minutes (`--timeout 9m`; the Bash call's `timeout` is `600000`). A longer task comes back as `running` with a handle, and the agent follows it.
- **Cost.** The footer shows what a native turn cost (`unpriced` for a plan sign-in). Cursor and Grok report none.

## Permissions (default permission mode)

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

Dispatch several agents in one message, each in the background (`run_in_background`); each runs its own `craze new --wait` with a Bash `timeout` of `600000`. Their results arrive as each finishes. Give each a different task or a different model; do not point two at the same files to edit.

## Driving craze directly

Without the agents (for example from your own Bash call), the commands are these. **Every Bash call that waits must set `timeout: 600000`** (the default is 120 s) and use `--timeout 9m` under it; add `--max-message 20000` so a long answer does not overflow the Bash output (the rest goes to a file named in the output).

```bash
craze new --wait --timeout 9m --max-message 20000 --provider native --model chatgpt/gpt-6.1-sol --effort high --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze send 175bfe79 --wait --timeout 9m --max-message 20000 --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze wait 175bfe79 --from <handle> --timeout 9m --max-message 20000
```

Write the prompt to a file with `Write` and pass it with `--prompt-file`. Keep the command plain: no variables, `$(...)`, heredoc or `||`. The full command list, statuses, exit codes and handles are in [references/commands.md](references/commands.md).

## Watching and stopping

- `craze attach --session <short>` opens the session's TUI (the footer's last line prints the exact command). It is how a person answers a `needs_input` ask or watches the work.
- `craze ps` lists the machine's sessions.
- `craze cancel <short>` cancels its running turn; the session goes on. `craze stop <short>` ends the session.
- A session a delegation started is marked in the index: `craze -c` skips it, and the resume picker and session list dim it.
