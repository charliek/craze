# Claude Code sub-agents

This repository is a Claude Code plugin. It adds sub-agents that hand a task to
a craze session — on craze's own **native** provider, **Grok** or **Cursor** —
and return the session's result to Claude Code, the way a native Claude Code
sub-agent returns its own. Use it to put another model on a task, to get a
second opinion, or to run several models side by side.

It needs **craze 0.2.0 or newer** on your `PATH`, signed in to the providers
you use ([Quick Start](quick-start.md)). An older craze fails with `this craze
is too old; the plugin needs craze ≥ 0.2.0`.

## Install

In Claude Code:

```text
/plugin marketplace add charliek/craze
/plugin install craze@craze
```

## The agents

Dispatch one with the Agent tool: `subagent_type` `craze:<name>`, and the task
as the prompt.

| Agent | Runs | Use for |
|-------|------|---------|
| `craze:delegate` | the provider and model you name | anything the presets do not cover |
| `craze:sol` | native `chatgpt/gpt-6.1-sol`, effort high | hard coding, debugging and review |
| `craze:astra` | native `chatgpt/gpt-6-astra`, effort high | the hardest design and review questions |
| `craze:muse` | native `muse-spark-1.3-contributor` | routine, bounded tasks, cheaply |
| `craze:grok` | grok `grok-4.7`, effort high | Grok's own agent |
| `craze:cursor` | cursor `composer-2.5` | Cursor's agent |

`craze:delegate` reads a first line naming the target, then the task:

```text
target: native chatgpt/gpt-6.1-sol effort=high mode=plan
Review internal/cache/evict.go for races. Report each one with the line and a fix.
```

The provider is `native`, `cursor` or `grok`; the model, effort (`low`,
`medium`, `high`, `xhigh`, `max`) and mode (`plan` or `ask`) are optional. The
presets take the task alone, or an optional first line `mode=plan` or
`mode=ask`.

The agents run on Haiku and only forward: each writes the task to a file under
`/tmp`, runs one `craze new --wait` ([reference](../reference/cli.md#craze-new-wait))
and returns what it printed. They never do the task themselves, and never retry
on another model. The session works in Claude Code's current directory, with
its own model's tools.

The task must stand alone: the session has not seen the conversation. Give it
the goal, the paths and what to return, and put a large context in a file in
the project that the task names.

### The result

The agent returns the turn's last message and a footer:

```text
The flaky test raced the cache's eviction; I made eviction wait for readers.
---
completed · 27s · 5 tool calls · $0.04 · context 16.6k/272k
files: stats/core.py
session 175bfe79 · craze attach --session 175bfe79
```

The status is `completed`, `failed`, `cancelled`, `dropped`, `needs_input` or
`running`, and the command's exit code follows it ([Waiting
commands](../reference/cli.md#waiting-commands)). A message over 20,000
characters is cut and the whole result written to a file the output names, so a
long answer does not overflow Claude Code's Bash output.

### Long tasks and follow-ups

A call waits up to nine minutes (`--timeout 9m`, under a Bash `timeout` of
`600000` — Claude Code's default is two minutes). A longer task comes back
`running` with a handle, and the agent follows it with `craze wait <short>
--from <handle>`; it asks again at most three times, so about 36 minutes in all.

SendMessage to the agent continues the same session with its context: the
agent runs `craze send <short> --wait` ([reference](../reference/cli.md#craze-send)).
A session whose host has since exited is loaded again from the session index.

To watch or answer a session, run `craze attach --session <short>` (the footer
prints it). A session that stops on a question or a permission request that
nothing answers reports `needs_input` (exit 4).

### Running several at once

Dispatch several agents in one message, each in the background. Each is its own
session; their results arrive as each finishes. Give them different tasks or
different models, and do not point two at the same files to edit.

## Plan and ask modes

| Mode | Edits | The shell | The turn |
|------|-------|-----------|----------|
| none | allowed | allowed | runs to its end |
| `mode=ask` | refused | refused | runs to its end |
| `mode=plan` | refused, except the agent's own plan file | allowed | **ends at the plan** |

With `mode=plan` the plan comes back in the result (`plan:`), the status is
`completed`, and nothing is implemented — on every provider. The session stays
in plan mode, follow-ups and reloads included, so a follow-up asking it to
implement gets another plan. To carry the plan out, start a new delegation
without `mode=plan` and put the plan in its task. A task that says "read-only" or "do not edit"
is a request to the model, not something craze enforces; only `mode=ask`
removes the shell.

## Permissions

Claude Code's permission rules and hooks apply to Claude Code's own tools. A
craze session's tools run inside craze, so **they do not apply to them**. Only
the mode above limits the session, and it asks nothing of you while it works:
questions it asks are answered with their first option and a plan it presents is
returned, not accepted. Do not delegate what you would not let that model do
unattended in this directory.

In Claude Code's default permission mode the agents themselves run `craze` and
write the task file, so each call prompts unless you allow them in your
settings:

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

## Against a native Claude Code sub-agent

| Behaviour | Native sub-agent | craze sub-agent |
|-----------|------------------|-----------------|
| Dispatched with the Agent tool | yes | yes (`craze:<name>`) |
| Works in the caller's directory | yes | yes |
| Read, edit, search and shell tools | Claude Code's | the provider's own |
| Reads `CLAUDE.md` and rules | yes | native yes; cursor and grok read their own |
| Background run, completion notice | yes | yes |
| The caller's context stays clean | yes | yes: the last message and a footer |
| SendMessage continues with context | yes | yes, also after the host has exited |
| TaskStop stops the work | yes | stops the waiting command, which cancels its own turn when it gets SIGINT, SIGTERM or SIGHUP; if the turn runs on, `craze cancel <short>` or `craze stop <short>` ends it |
| Turn longer than ten minutes | yes | `running`, then `craze wait` |
| Watch it live | transcript view | `craze attach --session <short>` |
| Cost reported | yes | native only (`unpriced` on a ChatGPT plan); cursor and grok report none |
| Claude Code permission rules and hooks | apply | **do not apply** |
| Read-only | a `tools` allow-list | `mode=plan` or `mode=ask` (not a sandbox) |
| Asks during the turn | Claude Code's own flow | answered by craze's policy; a permission is `needs_input` |
| Shows in `craze -c` | n/a | no: `craze -c` skips it, and the resume picker and session list show it dimmed or tagged |
| Overhead per call | none | a few seconds, plus a Haiku turn to forward |

A session left running when Claude Code is interrupted keeps going until its
turn ends; `craze ps` lists it and `craze stop <short>` ends it.
