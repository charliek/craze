# Claude on your plan

craze can drive **your own Claude Code, on your Claude Pro or Max plan**: the
same streaming transcript, tool rows, permission cards, resume and detached
hosts as its other providers, with Claude Code doing the work. craze runs the
[`claude-agent-acp`](https://www.npmjs.com/package/@agentclientprotocol/claude-agent-acp)
adapter, the Agent Client Protocol project's bridge to Claude Code, and the
adapter runs your signed-in `claude`.

!!! note "A preview"
    The provider is reached with `--provider claude` and is not yet listed by
    `craze providers`, the picker or `--help`. Its question and plan cards,
    modes (`Shift+Tab` cycles Manual, accept edits and plan) and interjections
    work ([Cards](../reference/tui.md#cards)): as in Claude Code, `Esc` on a
    question cancels it, aborting the tool that asked. Sub-agents and
    background work work too ([below](#background-work-and-sub-agents)); plan
    usage arrives in a later release, and it is listed then.

## Set it up

1. **Install Claude Code** and sign in with your Claude plan:

    ```shell
    claude auth login
    ```

2. **Install the adapter** (Node 22 or newer):

    ```shell
    npm i -g @agentclientprotocol/claude-agent-acp
    ```

3. **Start a session:**

    ```shell
    craze --provider claude
    craze prompt --provider claude "explain this repository"
    ```

craze finds the adapter on `PATH` (or `[agents].claude` in `config.toml`, or
`--agent-bin`), and `claude` on `PATH`. A `claude` that is not on `PATH` is
named with `CLAUDE_CODE_EXECUTABLE`:

```shell
CLAUDE_CODE_EXECUTABLE=/opt/claude/bin/claude craze --provider claude
```

craze needs adapter **0.107.0** and Claude Code **2.1.322** at least, and was
tested with adapter **0.107.3** and Claude Code **2.1.326**. `claude update`
and the `npm i -g` above bring them up to date.

## What craze runs, and what it never touches

- craze runs `claude --setting-sources "" auth status --json`, `claude
  --version` and `claude-agent-acp --version` before each session, then the
  adapter, which runs `claude`.
- craze **holds no credential, never logs you in, and reads nothing under
  `~/.claude`.** Your login stays Claude Code's.
- Your Claude Code setup applies — settings, hooks, permission rules,
  `CLAUDE.md`, skills, MCP servers — except that craze starts each session in
  Manual (or `plan` under `--plan`), keeps fast mode off, and owns the
  session's model. [What craze sets](../reference/configuration.md#what-craze-sets)
  lists each.

## Plan only

A claude session runs only on a **Claude Pro or Max plan**. Before the first
prompt craze checks the saved login (`claude auth status`), and then the login
the adapter says it would bill — which sees your project's settings, so an
`apiKeyHelper` there counts. An API key, a cloud or gateway login, an OAuth
token or an enterprise seat refuses the start with an `auth` error naming
what was found; if the adapter never says, the start fails rather than guess.

craze also removes from the adapter's environment every variable that would
route billing elsewhere (`ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, the
Bedrock, Vertex and Foundry switches, model pins, …).
[The environment](../reference/configuration.md#the-environment) has the whole
list. That covers the process environment only: an `env` block in a Claude
`settings.json`, or managed policy, is Claude Code's and still applies, which
is why the adapter's own report is checked too.

Fast mode bills as extra usage on a Claude plan, so craze refuses `--fast`
with claude and turns fast mode off whenever it finds it on.

## Background work and sub-agents

Claude Code's sub-agents (`Agent`) show as rows of their own under the status
rows, nested when one starts another, each with its own transcript
([Claude's sub-agents](../reference/tui.md#claude-sub-agents)). A child that
asks for permission or a question raises its own card, labelled with the
child; your turn ending does not close it. `Delete` on a running row stops
that child alone.

**Held turns.** When a task starts a background sub-agent, Claude keeps your
turn open until the sub-agent and Claude's reply about it are done. So
`craze new --wait`, and `craze send --wait --exit-after-turn`, return the
whole job, the sub-agent's report included, not just the moment it was
launched.

**Background commands** (`run_in_background`) are different: Claude's turn
ends as soon as the command starts, and craze does not track the command.
It does not keep your turn, a `--wait`, or the host open. When it finishes
while the session is still running, Claude reports on it in a turn of its
own, which craze shows as `the agent continued on its own (background
work)`. A host that exits — `--exit-after-turn`, an idle exit, `craze stop` —
ends the adapter, Claude Code and any commands still running; an unattended
session (`craze new --wait`) also turns off Claude Code's `ScheduleWakeup`,
`Monitor` and `CronCreate`, which would never fire after the host goes.

**Turns of Claude's own.** ACP v1 marks no start or end for these, so craze
infers them ([the rules](../reference/tui.md#claude-working-on-its-own)): such
a turn ends 2 seconds after Claude's usage report for it, or after a minute
with no word from Claude. A message you type meanwhile is queued and sent once
it ends.

**Cancel.** `Esc` (or `craze cancel`) stops your turn, or a turn Claude is
running on its own. It does not stop background commands, or sub-agents
working in the background: Claude keeps them running, as Claude Code does.
Stop a sub-agent with `Delete` on its row.

## When a start fails

[Claude sessions](../reference/cli.md#claude-sessions) lists each failure, its
words and its kind. The common ones:

- ``Claude Code is not signed in (run `claude auth login`)``;
- `craze runs claude on a Claude Pro or Max plan only, and this login is …`:
  sign in to a Pro or Max plan;
- `claude-agent-acp … is older than craze needs`: run the `npm i -g` above;
- `agent binary not found: claude-agent-acp is not on PATH`: install the
  adapter, or set `[agents].claude`.
