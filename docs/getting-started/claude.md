# Claude on your plan

craze can drive **your own Claude Code, on your Claude Pro or Max plan**: the
same streaming transcript, tool rows, permission cards, resume and detached
hosts as its other providers, with Claude Code doing the work. craze runs the
[`claude-agent-acp`](https://www.npmjs.com/package/@agentclientprotocol/claude-agent-acp)
adapter, the Agent Client Protocol project's bridge to Claude Code, and the
adapter runs your signed-in `claude`.

claude is listed after grok — in `craze providers`, the startup picker,
`/provider` and `--help` — and reached with `--provider claude` too. Its
question and plan cards, modes (`Shift+Tab` cycles Manual, accept edits and
plan) and interjections work ([Cards](../reference/tui.md#cards)): as in
Claude Code, `Esc` on a question cancels it, aborting the tool that asked.
Sub-agents and background work work too
([below](#background-work-and-sub-agents)), and the status row shows your
plan's usage ([below](#usage-and-your-plans-limits)).

!!! warning "Turn off extra usage on your Claude account"
    craze stops a session that Claude says is billing extra usage, but that
    is **containment, not prevention**: the request that tells craze has
    already been billed. The only hard guarantee is extra usage turned off
    on your Claude account. [Extra usage](#extra-usage-the-overage-guard)
    says what craze does and what it cannot do.

## Set it up

1. **Install Claude Code** and sign in with your Claude plan:

    ```shell
    claude auth login
    ```

2. **Install the adapter** (Node 22 or newer):

    ```shell
    npm i -g @agentclientprotocol/claude-agent-acp
    ```

3. **Start a session:** pick `claude` in the startup picker, or name it:

    ```shell
    craze --provider claude
    craze prompt --provider claude "explain this repository"
    ```

    `craze providers` says whether both binaries were found, and names the
    one missing.

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

## Usage and your plan's limits

The status row's usage part, on a claude session, reads like
`3% ctx · 59.6k / 122k tok · max · 7d 45%`: how full the context is, the last
turn's tokens and the session's (as Claude counts them; **never dollars** — the
adapter's cost figure is what the API would charge, not what your plan pays),
your plan, and how much of its current limit window is used: `5h` for the
five-hour window, `7d` for a seven-day one. Claude reports the window only when
it changes, so the figure is the last one it sent
([the usage part](../reference/tui.md#usage-and-cost)).

**Plan mode runs on Sonnet,** whatever model you picked: Claude Code switches
to it for planning, and it spends your plan accordingly. When Claude's usage
reports name another model family than the one picked, the status row says
the one in use first: `sonnet (in use; picked Haiku 5.5 · low)`.

## Extra usage: the overage guard

A Claude plan can be set to keep going past its limit as **extra usage**,
billed on top of the plan. craze runs claude on the plan alone, so it watches
for the signs that a request was, or would be, billed that way — Claude's own
report that extra usage is in use, a limit window that is the extra-usage one,
or the plan's limit reached while extra usage is allowed — on every usage
report, whether you prompted, Claude is working on its own, or nothing is
running, and on a prompt's rate-limit failure. When one arrives craze:

1. **writes a trip record**, `claude-overage.json` in craze's directory
   (`~/.craze/claude-overage.json`, or under `$CRAZE_HOME`), holding until the
   limit resets (an hour on when Claude gives no reset time);
2. **ends the running turn** — yours, or one Claude started on its own — with
   a `quota` error: *Claude would bill this as extra usage: your plan's 7-day
   limit is reached (resets …). craze stopped this session; the request in
   flight may already have been billed.*;
3. **stops the session:** it cancels the turn, gives Claude 2 seconds, then
   kills the adapter's process group — Claude Code and every sub-agent and
   background command with it. Nothing Claude sends after that is shown. The
   session is still yours: `craze -c` resumes it once the limit has reset.

**The record is machine-wide.** Until it expires, every claude session start
and every claude prompt on this machine — another project's too — is refused
with `quota`, naming the reset time. It expires on its own; if you turned
extra usage off, or your limit reset early, delete the file to go on at once.

**What craze cannot do.** It is containment, not prevention:

- the request that carries the sign has already been billed, and so may the
  one in flight when craze stops the session;
- a session that starts while the account is already in extra usage gets no
  sign until its first usage report, so that first request is billed;
- work Claude does that craze never hears about cannot be stopped by it.

**Turn off extra usage on your Claude account** (claude.ai, under your plan's
usage settings): that is the only hard guarantee, and craze recommends it for
every account it drives.

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
  adapter, or set `[agents].claude`;
- `Claude would bill this as extra usage: …`: the overage guard's trip record
  holds ([above](#extra-usage-the-overage-guard)).
