# Claude Code sub-agents

This repository is a Claude Code plugin. It adds sub-agents that hand a task to
a craze session — on craze's own **native** provider, **Grok** or **Cursor** —
and return the session's result to Claude Code, the way a native Claude Code
sub-agent returns its own. Use it to put another model on a task, to get a
second opinion, or to run several models side by side.

Its skill (`skills/craze`) is provider-neutral: it shows any agent — Claude
Code, Cursor, Grok or a craze session — how to hand work to another provider
through craze, [Claude included](#claude-as-a-target), and never to its own
([Don't call yourself](#dont-call-yourself)).

It needs **craze 0.2.0 or newer** (v0.1.x is too old) on your `PATH`, signed in to the providers
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
| `craze:sol` | native `chatgpt/gpt-6.2-sol`, effort high | hard coding, debugging and review |
| `craze:astra` | native `chatgpt/gpt-6.2-astra`, effort high | the hardest design and review questions |
| `craze:muse` | native `muse-spark-1.3-contributor` | routine, bounded tasks, cheaply |
| `craze:wonder` | native `muse-wonder-1.0`, effort high | hard tasks, at about three times Spark's price per token |
| `craze:grok` | grok `grok-4.7`, effort high | Grok's own agent |
| `craze:neon` | antigravity `gemini-4-neon`, effort high | Antigravity's agent (Gemini 4 Neon); no `mode=ask` |
| `craze:cursor` | cursor `composer-2.5` | Cursor's agent |

`craze:delegate` reads a first line naming the target, then the task:

```text
target: native chatgpt/gpt-6.2-sol effort=high mode=plan
Review internal/cache/evict.go for races. Report each one with the line and a fix.
```

The provider is `native`, `cursor`, `grok`, `claude` or `antigravity` (`claude` and
`antigravity` take no `mode=ask`, and craze refuses `claude` from Claude Code itself:
[below](#dont-call-yourself));
the model, effort (`low`, `medium`, `high`, `xhigh`, `max`) and mode (`plan` or
`ask`) are optional. The
native presets take the task alone, or an optional first line of `mode=plan` or
`mode=ask` and `web=on` or `web=off`, in either order; `craze:grok` and `craze:cursor` take
`mode=` only, and `craze:neon` takes `mode=plan` only: it answers `mode=ask` with a refusal and
runs nothing. An Antigravity session's asynchronous sub-agents may still be running when the
turn ends, and their results are not in that turn's result.

`web=on` (native only, `target:` line included: `target: native … web=on`) gives the session
web search and page fetch, for information newer than the model's training or not in the
repository. Queries and fetched URLs go to Exa, so leave it off for confidential work. With no
`web=` the home's own setting applies. `web=off` guarantees no web tools. It needs a craze newer
than 0.2.0; the choice is fixed when the session is created. See
[`--web-search`](../reference/cli.md#web-search-flags) and the result's `webSearch` object.

`craze:sol` and `craze:astra` run on a [ChatGPT plan](../reference/configuration.md#the-chatgpt-plan)
sign-in. With an [OpenAI API key](../reference/configuration.md#openai)
instead, billed per token, name the key's model with `craze:delegate`:

```text
target: native openai/gpt-6.2-sol effort=high
Review internal/cache/evict.go for races. Report each one with the line and a fix.
```

The agents run on Haiku and only forward: each writes the task to a file under
`/tmp`, runs one `craze new --wait` ([reference](../reference/cli.md#craze-new-wait))
and returns what it printed. They never do the task themselves, and never retry
on another model. The session works in Claude Code's current directory, with
its own model's tools.

The task must stand alone: the session has not seen the conversation. Give it
the goal, the paths and what to return, and put a large context in a file in
the project that the task names. The task itself can be up to 512 KiB (32 KiB on
craze 0.2.0; [the prompt](../reference/cli.md#the-prompt)), but a smaller one is
cheaper: a native turn sends it again with every request.

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
A follow-up sent while the session is still working (after the agent gave up
on a `running` turn, say) waits behind that turn, and one over 32 KiB is
refused, exit 1, which the agent passes back: `the prompt was refused: it is
over 32 KiB, the most a prompt can wait in the session's queue, and the session
was busy; send it once the turn has ended`. Send it once the turn has ended, or
put the bulk in a file.

To watch or answer a session, run `craze attach --session <short>` (the footer
prints it). A session that stops on a question or a permission request that
nothing answers reports `needs_input` (exit 4).

### Running several at once

Dispatch several agents in one message, each in the background. Each is its own
session; their results arrive as each finishes. Give them different tasks or
different models, and do not point two at the same files to edit.

## Don't call yourself

craze is for reaching a **different** provider. An agent never uses it to
start a session on the provider it already runs on; its own sub-agent tool
does that better:

| Caller | Its provider | Use instead of craze |
|--------|--------------|----------------------|
| Claude Code | `claude` | the Agent tool |
| Cursor | `cursor` | Cursor's own sub-agents |
| Grok | `grok` | Grok's own sub-agents |
| Antigravity (`agy`) | `antigravity` | agy's own sub-agents |
| a craze native session | `native` | the `agent` tool, which runs any native model (`craze:sol`'s included) |

Every agent file opens with this rule, and there is no `craze:claude` preset:
only Claude Code loads the agents, and it must not target claude. Where craze
can tell who is calling, [`craze new`](../reference/cli.md#craze-new) refuses
the caller's own provider, exit 2: `craze: this agent already runs on claude;
use your own sub-agent tool for claude work, not craze (--allow-same-provider
overrides)`. It knows a craze session by `CRAZE_SESSION_PROVIDER` (craze sets
it for every agent it starts, native's shell included), Claude Code by
`CLAUDECODE=1`, a standalone `cursor-agent` by `CURSOR_AGENT` and a standalone
`agy` by `ANTIGRAVITY_AGENT`. With two or more of those and no
`CRAZE_SESSION_PROVIDER`, one started the other and the caller is
unknown, so craze does not refuse. Grok sets no marker craze
trusts, so from Grok the rule is the text alone. The agents return the
refusal as it is and do not retry; `--allow-same-provider` is for a person who
means it.

## Claude as a target

An agent that does not run on Claude — Cursor, Grok, or a native, cursor or
grok craze session — reaches [Claude on your plan](claude.md) through the
skill's direct commands (it needs a craze newer than 0.3.0):

```bash
craze new --wait --timeout 9m --max-message 20000 --provider claude --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
```

A claude session is your own Claude Code: it runs under your Claude Code
settings, hooks and permission rules (the project's included), its turns are
unpriced and count against your plan's limits, and it has no `--ask` mode,
web search or `--fast`. A background sub-agent of Claude's is waited for, but
a background shell command (`run_in_background`) is not: the turn ends once
the command starts.

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
native, Grok or Cursor session's tools run inside craze, so **they do not
apply to them** (a claude session, reached from another agent, is your own
Claude Code and runs under your own rules and hooks). Only
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
| TaskStop stops the work | yes | cancels its craze turn: TaskStop signals the waiting command, which cancels its own turn (should it say the cancel was not confirmed, `craze cancel <short>`); the session's host is not in that command's process tree, so the session stays, and SendMessage continues it. `craze stop <short>` ends the session |
| Turn longer than ten minutes | yes | `running`, then `craze wait` |
| Watch it live | transcript view | `craze attach --session <short>` |
| Cost reported | yes | native (`unpriced` on a ChatGPT plan); claude: unpriced, on your Claude plan; cursor and grok report none |
| Claude Code permission rules and hooks | apply | **do not apply** |
| Read-only | a `tools` allow-list | `mode=plan` or `mode=ask` (not a sandbox) |
| Asks during the turn | Claude Code's own flow | answered by craze's policy; a permission is `needs_input` |
| Shows in `craze -c` | n/a | no: `craze -c` skips it, and the resume picker and session list show it dimmed or tagged |
| Overhead per call | none | a few seconds, plus a Haiku turn to forward |

A native session also reads Claude's own agent definitions (`.claude/agents`).
In one, a bare `WebSearch` or `WebFetch` in `tools` now names craze's native
`websearch` and `webfetch` when the session is offered them ([Native web
search](../reference/configuration.md#native-web-search)), and is dropped as
before when it is not. A restricted entry such as `WebFetch(domain:example.com)`
is refused in `tools` (craze has no domain restriction) and removes the whole
tool in `disallowedTools`.

A session left running when Claude Code is interrupted keeps going until its
turn ends; `craze ps` lists it and `craze stop <short>` ends it.
